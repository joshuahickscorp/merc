package main

// Selector scale-curve harness (Step 20 digital twin shape).
//
// Measures the production SQL entry points against synthetic fleets in a real
// PostgreSQL. There is no Go re-implementation of ranking: every timed call is
// ClaimTasksTx, AuthorizeRealtimeContract, or CreateServiceLease.
//
// Opt-in only — never part of the normal make test / make ci gate:
//
//	MERC_SELECTOR_SCALE_CURVE=1 \
//	MERC_TEST_DATABASE_URL=postgres://cx:cx@localhost:5432/cx?sslmode=disable \
//	bash scripts/with-isolated-test-db.sh \
//	  bash -c 'cd control && go test -count=1 -run '^TestSelectorScaleCurve$' -timeout 6h .'
//
// Writes evidence/perf/selector-scale-curve.json.
//
// Independent variables (do not average across lanes):
//   batch         — fleet workers online (EXISTS cheaper_class/ask scans)
//   realtime      — offer-book size for one runtime_profile_id + sha
//   service_lease — offer-book size for one profile + region under FOR UPDATE

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	selectorScaleSeed    int64 = 20260809
	selectorScaleEnv           = "MERC_SELECTOR_SCALE_CURVE"
	selectorScaleSamples       = 40
	// Per concurrency cell: discard this many leading calls after seed/EXPLAIN so
	// plan compilation and cold shared-buffers do not enter the timed percentiles.
	selectorScaleWarmup = 8
	// Extra warm-up calls on the FIRST timed scale of each lane only (on top of
	// the per-cell warm-up). Recorded in the artifact — never silently dropped.
	selectorScaleLaneFirstPointWarmup = 12
	selectorScaleConcurrencyHigh      = 8 // multi-poller contention; not host saturation
	selectorScaleJobBacklog           = 200
	selectorScaleCopyChunk            = 20_000
	// Production supplier cadence: agent/src/vllm.rs HEARTBEAT_INTERVAL is 15s
	// against the 45s liveness window. The harness beats at the same rate so it
	// measures selection over a live book, not over a book it let die.
	selectorScaleHeartbeatInterval = 15 * time.Second
	// The window the heartbeat is racing: realtime_store.go:1099 and the lease
	// offer predicate both admit supply seen within 45 seconds.
	selectorScaleLivenessWindow = 45 * time.Second
	selectorScaleEvidenceRel    = "evidence/perf/selector-scale-curve.json"
	// A narrowed run writes here so it can never be mistaken for, or overwrite,
	// the curve.
	selectorScaleDiagnosticEvidenceRel = "evidence/perf/selector-scale-diagnostic.json"
)

var selectorScaleLadder = []int{1_000, 10_000, 100_000, 1_000_000}

// A full ladder is 1.4 hours. Diagnosing one bad cell should not cost that, and
// the first run's realtime failure was diagnosed by re-reading an artifact
// rather than by asking the one question that would have answered it — because
// asking it meant buying the whole campaign again.
//
// These narrow the run. A narrowed run is NOT a scale curve, so the artifact
// records the filter and marks itself partial; nothing downstream may read a
// diagnostic artifact as the curve.
const (
	selectorScaleLanesEnv  = "MERC_SELECTOR_SCALE_LANES"
	selectorScaleLadderEnv = "MERC_SELECTOR_SCALE_LADDER"
)

func selectorScaleSelectedLanes() ([]string, bool) {
	all := []string{"batch", "realtime", "service_lease"}
	raw := strings.TrimSpace(os.Getenv(selectorScaleLanesEnv))
	if raw == "" {
		return all, false
	}
	allowed := map[string]bool{}
	for _, l := range all {
		allowed[l] = true
	}
	var out []string
	for _, want := range strings.Split(raw, ",") {
		want = strings.TrimSpace(want)
		if allowed[want] {
			out = append(out, want)
		}
	}
	if len(out) == 0 {
		return all, false
	}
	return out, true
}

func selectorScaleSelectedLadder(t *testing.T) ([]int, bool) {
	raw := strings.TrimSpace(os.Getenv(selectorScaleLadderEnv))
	if raw == "" {
		return selectorScaleLadder, false
	}
	var out []int
	for _, field := range strings.Split(raw, ",") {
		field = strings.TrimSpace(field)
		if field == "" {
			continue
		}
		n, err := strconv.Atoi(field)
		if err != nil || n <= 0 {
			t.Fatalf("%s=%q: %q is not a positive scale", selectorScaleLadderEnv, raw, field)
		}
		out = append(out, n)
	}
	if len(out) == 0 {
		return selectorScaleLadder, false
	}
	sort.Ints(out)
	return out, true
}

// Bible scale targets (recorded as targets only; harness never marks them met).
var selectorScaleTargetsMS = map[int]float64{
	1_000: 1, 10_000: 3, 100_000: 10, 1_000_000: 25,
}

func TestSelectorScaleCurve(t *testing.T) {
	if os.Getenv(selectorScaleEnv) != "1" {
		t.Skip("set MERC_SELECTOR_SCALE_CURVE=1 to run the production-SQL selector scale curve")
	}
	installSettlementCurrencyForTest(t, "usd")
	t.Setenv("MERC_TOKEN_KEY", "selector-scale-curve-key-32bytes-min!!!!")

	loadBefore, _ := readLoadAverage()
	host, _ := os.Hostname()
	t.Logf("selector-scale-curve start host=%s cpus=%d load=%v seed=%d",
		host, runtime.NumCPU(), loadBefore, selectorScaleSeed)

	store, pool := openSelectorScaleStore(t, 32)
	ctx := context.Background()

	// Eligibility smoke: 32-worker fleet must claim / authorize / lease successfully
	// before any scale ladder point is trusted.
	if err := verifySelectorScaleEligibility(t, ctx, store, pool); err != nil {
		t.Fatalf("eligibility smoke failed (harness not measuring claim-eligible rows): %v", err)
	}

	profile := sortedVLLMProfiles()[0]
	commit := mercSourceCommitSHA()
	startedAt := time.Now().UTC()

	report := selectorScaleReport{
		SchemaVersion: 1,
		GeneratedAt:   startedAt.Format(time.RFC3339),
		SourceCommit:  commit,
		Seed:          selectorScaleSeed,
		Host:          host,
		NumCPU:        runtime.NumCPU(),
		Invocation: selectorScaleInvocation{
			EnvGate:                selectorScaleEnv + "=1",
			Command:                "bash scripts/with-isolated-test-db.sh bash -c 'cd control && go test -count=1 -run ^TestSelectorScaleCurve$ -timeout 6h .'",
			ExcludedFromNormalGate: true,
			ExclusionProof:         "TestSelectorScaleCurve skips unless MERC_SELECTOR_SCALE_CURVE=1; listed in scripts/allowed-test-skips.txt; make test / make ci never set the env var",
		},
		Honesty: selectorScaleHonesty{
			WhatThisProves:       "algorithmic scaling of the production SQL under synthetic load on one host",
			WhatThisDoesNotProve: "physical fleet performance, or any claim about a real network",
			Contention:           "UNMEASURED until run completes",
		},
		MeasurementProtocol: selectorScaleMeasurementProtocol{
			PerCellWarmupCalls:        selectorScaleWarmup,
			LaneFirstPointExtraWarmup: selectorScaleLaneFirstPointWarmup,
			WarmupPolicy:              "Each concurrency cell discards the first N production-SQL calls serially (plan compile + cold cache) before timed samples. The first scale point of each lane discards an additional M calls on c=1. Discarded latencies are recorded under each point's warmup field — never silently dropped. Timed wall_ms percentiles are SUCCESS-only and exclude warm-up calls. Realtime samples FinalizeRealtimeFailure after each auth so reserved credit cannot accumulate into later fast funding failures.",
			AxisLabelling:             "batch.independent_variable = fleet_workers_online (EXISTS cheaper_class/ask scan cost). realtime and service_lease independent_variable = offer-book size for the profile (and region for leases) under test; the harness seeds that many offers on the measured profile, so scale is NOT ambient fleet size for those lanes.",
			RealtimeAxisHypothesis:    "REFUTED as a mislabel: AuthorizeRealtimeContract scopes offers by runtime_profile_id + runtime_profile_sha256, and this harness seeds `scale` ACTIVE offers on the profile under test (workers==offers==scale). The x-axis for realtime/service_lease is offer-book size, which is what the SQL scans. Growing an unrelated fleet would not grow the book — that is why the IV is the book, not fleet size.",
			TargetMetPolicy:           "Harness always writes target_met_judgement=NOT_JUDGED_BY_HARNESS. Reviewer fills reviewer_verdict.",
		},
		TargetsMS: selectorScaleTargetsMS,
		Lanes:     map[string]selectorScaleLane{},
	}

	lanes, lanesFiltered := selectorScaleSelectedLanes()
	ladder, ladderFiltered := selectorScaleSelectedLadder(t)
	if lanesFiltered || ladderFiltered {
		report.Partial = &selectorScalePartial{
			Filtered:           true,
			Lanes:              lanes,
			Ladder:             ladder,
			LanesEnv:           os.Getenv(selectorScaleLanesEnv),
			LadderEnv:          os.Getenv(selectorScaleLadderEnv),
			WhyThisIsNotACurve: "A filtered run measures the cells asked for and nothing else. It is a diagnostic. It may not be read as, compared against, or substituted for the full scale curve, and no target may be judged from it.",
		}
		t.Logf("DIAGNOSTIC RUN (not a curve): lanes=%v ladder=%v", lanes, ladder)
	}

	// Run lanes serially so fleet tables do not fight each other for RAM/disk.
	for _, lane := range lanes {
		laneReport, refusals := runSelectorScaleLane(t, ctx, store, pool, profile, lane, ladder)
		report.Lanes[lane] = laneReport
		report.Refusals = append(report.Refusals, refusals...)
		for _, p := range laneReport.Points {
			report.Defects = append(report.Defects, p.Defects...)
		}
	}

	loadAfter, _ := readLoadAverage()
	report.LoadAverageBefore = loadBefore
	report.LoadAverageAfter = loadAfter
	report.Honesty.Contention = formatSelectorScaleContention(loadBefore, loadAfter, runtime.NumCPU())
	report.FinishedAt = time.Now().UTC().Format(time.RFC3339)
	report.WallClockSeconds = time.Since(startedAt).Seconds()

	if err := writeSelectorScaleEvidence(report); err != nil {
		t.Fatalf("write evidence: %v", err)
	}
	wrote := selectorScaleEvidenceRel
	if report.Partial != nil {
		wrote = selectorScaleDiagnosticEvidenceRel
	}
	t.Logf("wrote %s (wall=%.1fs load_before=%v load_after=%v defects=%d)",
		wrote, report.WallClockSeconds, loadBefore, loadAfter, len(report.Defects))
}

// --- report types -----------------------------------------------------------

type selectorScaleReport struct {
	SchemaVersion       int                              `json:"schema_version"`
	GeneratedAt         string                           `json:"generated_at"`
	FinishedAt          string                           `json:"finished_at,omitempty"`
	WallClockSeconds    float64                          `json:"wall_clock_seconds,omitempty"`
	SourceCommit        string                           `json:"source_commit"`
	Seed                int64                            `json:"seed"`
	Host                string                           `json:"host"`
	NumCPU              int                              `json:"num_cpu"`
	LoadAverageBefore   []float64                        `json:"load_average_before"`
	LoadAverageAfter    []float64                        `json:"load_average_after"`
	Invocation          selectorScaleInvocation          `json:"invocation"`
	Honesty             selectorScaleHonesty             `json:"honesty"`
	MeasurementProtocol selectorScaleMeasurementProtocol `json:"measurement_protocol"`
	TargetsMS           map[int]float64                  `json:"bible_targets_ms"`
	Lanes               map[string]selectorScaleLane     `json:"lanes"`
	Refusals            []selectorScaleRefusal           `json:"refusals,omitempty"`
	Partial             *selectorScalePartial            `json:"partial_diagnostic_run,omitempty"`
	Defects             []selectorScaleDefect            `json:"defects,omitempty"`
}

// selectorScaleMeasurementProtocol records how contamination was handled so a
// discarded warm-up call is visible evidence, not an invisible omission.
type selectorScaleMeasurementProtocol struct {
	PerCellWarmupCalls        int    `json:"per_cell_warmup_calls"`
	LaneFirstPointExtraWarmup int    `json:"lane_first_point_extra_warmup_calls"`
	WarmupPolicy              string `json:"warmup_policy"`
	AxisLabelling             string `json:"axis_labelling"`
	RealtimeAxisHypothesis    string `json:"realtime_axis_hypothesis"`
	TargetMetPolicy           string `json:"target_met_policy"`
}

// selectorScalePartial marks an artifact produced by a narrowed run. Its
// presence is the refusal: a reader that finds this field is holding a
// diagnostic, not a curve.
type selectorScalePartial struct {
	Filtered           bool     `json:"filtered"`
	Lanes              []string `json:"lanes_measured"`
	Ladder             []int    `json:"ladder_measured"`
	LanesEnv           string   `json:"lanes_env,omitempty"`
	LadderEnv          string   `json:"ladder_env,omitempty"`
	WhyThisIsNotACurve string   `json:"why_this_is_not_a_curve"`
}

// selectorScaleDefect is a measured anomaly that must not be quoted as a
// selection-cost datum without the accompanying explanation.
type selectorScaleDefect struct {
	Lane     string `json:"lane"`
	Scale    int    `json:"scale"`
	Cell     string `json:"cell,omitempty"`
	Kind     string `json:"kind"`
	Summary  string `json:"summary"`
	Evidence string `json:"evidence"`
}

type selectorScaleInvocation struct {
	EnvGate                string `json:"env_gate"`
	Command                string `json:"command"`
	ExcludedFromNormalGate bool   `json:"excluded_from_normal_gate"`
	ExclusionProof         string `json:"exclusion_proof"`
}

type selectorScaleHonesty struct {
	WhatThisProves       string `json:"what_this_proves"`
	WhatThisDoesNotProve string `json:"what_this_does_not_prove"`
	Contention           string `json:"contention"`
}

type selectorScaleLane struct {
	EntryPoint              string                  `json:"entry_point"`
	IndependentVariable     string                  `json:"independent_variable"`
	IndependentVariableUnit string                  `json:"independent_variable_unit"`
	XAxisIsFleetSize        bool                    `json:"x_axis_is_fleet_size"`
	SQLTextDigest           string                  `json:"sql_text_digest"`
	SQLStatementName        string                  `json:"sql_statement_name"`
	Points                  []selectorScalePoint    `json:"points"`
	PlannerShapeFlips       []selectorScalePlanFlip `json:"planner_shape_flips,omitempty"`
}

type selectorScalePoint struct {
	Scale                    int                             `json:"scale"`
	IndependentVariableValue int                             `json:"independent_variable_value"`
	IndependentVariableName  string                          `json:"independent_variable_name,omitempty"`
	TargetMS                 float64                         `json:"target_ms"`
	TargetMetJudgement       string                          `json:"target_met_judgement"`
	Seed                     int64                           `json:"seed"`
	RowCounts                map[string]int64                `json:"row_counts"`
	CandidateStages          map[string]int64                `json:"candidate_counts_surviving_stages,omitempty"`
	SQLTextDigest            string                          `json:"sql_text_digest"`
	PlanShape                string                          `json:"plan_shape"`
	PlanShapeDetail          []string                        `json:"plan_shape_nodes,omitempty"`
	ExplainAnalyzeSummary    string                          `json:"explain_analyze_summary,omitempty"`
	ExplainBuffersSummary    string                          `json:"explain_buffers_summary,omitempty"`
	ExplainRawTruncated      string                          `json:"explain_raw_truncated,omitempty"`
	WallMS                   map[string]selectorScaleLatency `json:"wall_ms"`
	Warmup                   *selectorScaleWarmupEvidence    `json:"warmup,omitempty"`
	LoadAverageDuring        []float64                       `json:"load_average_during"`
	SeedWallSeconds          float64                         `json:"seed_wall_seconds"`
	MeasureWallSeconds       float64                         `json:"measure_wall_seconds"`
	DBBytesApprox            int64                           `json:"db_bytes_approx,omitempty"`
	Notes                    string                          `json:"notes,omitempty"`
	Status                   string                          `json:"status"` // measured | refused
	Defects                  []selectorScaleDefect           `json:"defects,omitempty"`
}

// selectorScaleWarmupEvidence makes discarded warm-up calls legible in the
// artifact so a cold first call cannot be mistaken for a missing measurement.
type selectorScaleWarmupEvidence struct {
	Policy                         string `json:"policy"`
	IsLaneFirstTimedPoint          bool   `json:"is_lane_first_timed_point"`
	PerCellWarmupCalls             int    `json:"per_cell_warmup_calls"`
	LaneFirstPointExtraWarmupCalls int    `json:"lane_first_point_extra_warmup_calls,omitempty"`
	// Discarded wall times by concurrency cell (includes lane-first extras on c=1).
	DiscardedByCell map[string]selectorScaleLatency `json:"discarded_by_cell_ms"`
	// Coldest discarded call vs first timed sample — proof cold-start was removed from percentiles.
	ColdestDiscardedMSByCell map[string]float64 `json:"coldest_discarded_ms_by_cell,omitempty"`
	FirstTimedSampleMSByCell map[string]float64 `json:"first_timed_sample_ms_by_cell,omitempty"`
	SampleFailuresInWarmup   map[string]int     `json:"sample_failures_in_warmup,omitempty"`
	SampleFailuresInTimed    map[string]int     `json:"sample_failures_in_timed,omitempty"`
	// Supplier-heartbeat sweeps run during the cell at the production cadence,
	// with their measured cost. Selection is timed against a live book, and the
	// cost of keeping it live is contention the artifact must show.
	HeartbeatSweepsByCell    map[string]int     `json:"heartbeat_sweeps_by_cell,omitempty"`
	HeartbeatMeanMSByCell    map[string]float64 `json:"heartbeat_mean_ms_by_cell,omitempty"`
	HeartbeatSlowestMSByCell map[string]float64 `json:"heartbeat_slowest_ms_by_cell,omitempty"`
	Note                     string             `json:"note"`
}

type selectorScaleLatency struct {
	P50 float64 `json:"p50"`
	P95 float64 `json:"p95"`
	P99 float64 `json:"p99"`
	Min float64 `json:"min"`
	Max float64 `json:"max"`
	N   int     `json:"n"`
}

type selectorScalePlanFlip struct {
	FromScale int    `json:"from_scale"`
	ToScale   int    `json:"to_scale"`
	FromShape string `json:"from_shape"`
	ToShape   string `json:"to_shape"`
	Note      string `json:"note"`
}

type selectorScaleRefusal struct {
	Lane               string  `json:"lane"`
	Scale              int     `json:"scale"`
	Reason             string  `json:"reason"`
	LargestCompleted   int     `json:"largest_completed_scale,omitempty"`
	RowsAtLargest      int64   `json:"rows_at_largest,omitempty"`
	BytesAtLargest     int64   `json:"bytes_at_largest,omitempty"`
	WallSecondsLargest float64 `json:"wall_seconds_at_largest,omitempty"`
	ObservedError      string  `json:"observed_error,omitempty"`
}

// --- store / eligibility ----------------------------------------------------

func openSelectorScaleStore(t *testing.T, maxConns int32) (*Store, *pgxpool.Pool) {
	t.Helper()
	previousActivation := activeRuntimeActivation.Load()
	t.Cleanup(func() { activeRuntimeActivation.Store(previousActivation) })
	base := requireTestDatabase(t)
	parsed, err := url.Parse(base)
	mustf(t, err, "parse MERC_TEST_DATABASE_URL: %v")
	name := "cx_sel_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:24]

	setupCtx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	admin := *parsed
	admin.Path = "/postgres"
	adminPool, err := pgxpool.New(setupCtx, admin.String())
	mustf(t, err, "connect postgres for CREATE DATABASE: %v")
	if _, err := adminPool.Exec(setupCtx, `CREATE DATABASE `+name); err != nil {
		adminPool.Close()
		t.Fatalf("create selector-scale database: %v", err)
	}
	adminPool.Close()
	t.Cleanup(func() {
		c, ccancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer ccancel()
		p, err := pgxpool.New(c, admin.String())
		if err != nil {
			return
		}
		defer p.Close()
		_, _ = p.Exec(c, `DROP DATABASE IF EXISTS `+name+` WITH (FORCE)`)
	})

	own := *parsed
	own.Path = "/" + name
	cfg, err := pgxpool.ParseConfig(own.String())
	must(t, err)
	cfg.MaxConns = maxConns
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	mustf(t, err, "connect selector-scale database: %v")
	t.Cleanup(pool.Close)
	store := NewStore(pool)
	migCtx, migCancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer migCancel()
	mustf(t, store.Migrate(migCtx), "migrate selector-scale database: %v")
	return store, pool
}

func verifySelectorScaleEligibility(t *testing.T, ctx context.Context, store *Store, pool *pgxpool.Pool) error {
	t.Helper()
	const n = 32
	profile := sortedVLLMProfiles()[0]

	// --- batch ---
	buyerID, err := store.CreateBuyerAccount(ctx, "sel-elig-"+uuid.NewString()+"@example.test", "integration-password", 100)
	if err != nil {
		return fmt.Errorf("batch buyer: %w", err)
	}
	fleet, err := seedSelectorBatchFleet(ctx, pool, n, selectorScaleSeed, 8, buyerID)
	if err != nil {
		return fmt.Errorf("batch seed: %w", err)
	}
	claimed, err := store.ClaimTasksTx(ctx, WorkerAuth{WorkerID: fleet.ClaimerWorkerID, SupplierID: fleet.ClaimerSupplierID})
	if err != nil {
		return fmt.Errorf("ClaimTasksTx: %w", err)
	}
	if claimed == nil {
		return errors.New("ClaimTasksTx returned nil on 32-worker eligible fleet")
	}
	t.Logf("eligibility batch: claimed task=%s job=%s", claimed.TaskID, claimed.JobID)

	// --- realtime ---
	rtBuyer, err := store.CreateBuyerAccount(ctx, "sel-rt-"+uuid.NewString()+"@example.test", "integration-password", 10_000)
	if err != nil {
		return fmt.Errorf("realtime buyer: %w", err)
	}
	if err := seedSelectorRealtimeBook(ctx, pool, profile, 16, selectorScaleSeed); err != nil {
		return fmt.Errorf("realtime seed: %w", err)
	}
	maxUSD, estUSD, maxPrompt, maxCompletion := realtimeAuthCeiling(t, profile, 7, 2)
	contract, _, err := store.AuthorizeRealtimeContract(ctx, RealtimeContractAuthorization{
		RequestID: "sel-elig-" + uuid.NewString(), BuyerID: rtBuyer, Profile: profile,
		InputCommitment: strings.Repeat("a", 64), RequestSHA256: strings.Repeat("b", 64),
		MaximumPriceUSD: maxUSD, EstimatedPriceUSD: estUSD, DeadlineAt: time.Now().Add(time.Minute),
		MaximumPromptTokens: maxPrompt, MaximumCompletionTokens: maxCompletion,
		EstimatedPromptTokens: 7, EstimatedCompletionTokens: 2,
		BuyerDeclaredCeilingUSD: maxUSD * 1.1,
	})
	if err != nil {
		return fmt.Errorf("AuthorizeRealtimeContract: %w", err)
	}
	if contract.WorkerID == uuid.Nil {
		return errors.New("AuthorizeRealtimeContract returned empty worker")
	}
	t.Logf("eligibility realtime: contract=%s worker=%s", contract.ID, contract.WorkerID)
	_, _ = store.FinalizeRealtimeFailure(ctx, contract.ID, uuid.New(), 500, 1, "selector_scale", "eligibility teardown", false)

	// --- service lease ---
	leaseBuyer := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO buyers (id,email) VALUES ($1,$2)`,
		leaseBuyer, leaseBuyer.String()+"@sel-lease.invalid"); err != nil {
		return fmt.Errorf("lease buyer: %w", err)
	}
	if err := store.SeedPrepaidBalance(ctx, leaseBuyer, 50_000_000_000, "sel-elig-"+leaseBuyer.String()); err != nil {
		return fmt.Errorf("lease prepaid: %w", err)
	}
	region := "ca-sel-elig"
	if err := seedSelectorLeaseBook(ctx, pool, profile, region, 16, selectorScaleSeed); err != nil {
		return fmt.Errorf("lease seed: %w", err)
	}
	lease, err := store.CreateServiceLease(ctx, leaseBuyer, ServiceLeaseRequest{
		RuntimeProfileID: profile.RuntimeProfileID, Region: region, Currency: "usd",
		MinimumReplicas: 1, MaximumReplicas: 1, TermSeconds: 60,
		MaximumP95LatencyMilliseconds: 500, BuyerDeclaredCeilingNanos: 135_000_000,
	})
	if err != nil {
		return fmt.Errorf("CreateServiceLease: %w", err)
	}
	if lease.WorkerID == uuid.Nil {
		return errors.New("CreateServiceLease returned empty worker")
	}
	t.Logf("eligibility service_lease: lease=%s worker=%s", lease.ID, lease.WorkerID)

	// Wipe smoke data so scale points start clean.
	if err := wipeSelectorScaleTables(ctx, pool); err != nil {
		t.Logf("truncate smoke tables (non-fatal if partial): %v", err)
	}
	return nil
}

// --- lane runner ------------------------------------------------------------

func runSelectorScaleLane(
	t *testing.T, ctx context.Context, store *Store, pool *pgxpool.Pool,
	profile VLLMRuntimeProfile, lane string, ladder []int,
) (selectorScaleLane, []selectorScaleRefusal) {
	t.Helper()
	var out selectorScaleLane
	var refusals []selectorScaleRefusal
	switch lane {
	case "batch":
		out.EntryPoint = "Store.ClaimTasksTx"
		out.IndependentVariable = "fleet_workers_online (EXISTS cheaper_class_online / cheaper_ask_online per eligible job)"
		out.IndependentVariableUnit = "workers_online"
		out.XAxisIsFleetSize = true
		out.SQLStatementName = "ClaimTaskSQL(claimed_by IS NULL)"
		out.SQLTextDigest = sqlTextDigest(ClaimTaskSQL("t.claimed_by IS NULL"))
	case "realtime":
		out.EntryPoint = "Store.AuthorizeRealtimeContract (offer-claim CTE)"
		out.IndependentVariable = "realtime_worker_offers rows for one runtime_profile_id + runtime_profile_sha256"
		out.IndependentVariableUnit = "offer_book_rows_on_profile"
		out.XAxisIsFleetSize = false
		out.SQLStatementName = "realtimeAuthorizeSelectOfferSQLSkip|Blocking"
		out.SQLTextDigest = sqlTextDigest(realtimeAuthorizeSelectOfferSQLSkip + "\n" + realtimeAuthorizeSelectOfferSQLBlocking)
	case "service_lease":
		out.EntryPoint = "Store.CreateServiceLease"
		out.IndependentVariable = "service_lease_worker_offers rows for one runtime_profile_id + region under FOR UPDATE"
		out.IndependentVariableUnit = "offer_book_rows_on_profile_region"
		out.XAxisIsFleetSize = false
		out.SQLStatementName = "CreateServiceLease ordered FOR UPDATE book walk"
		out.SQLTextDigest = sqlTextDigest(serviceLeaseBookWalkSQL())
	default:
		t.Fatalf("unknown lane %q", lane)
	}

	var prevShape string
	var prevScale int
	var lastCompleted int
	var lastRows int64
	var lastBytes int64
	var lastWall float64
	firstPoint := true

	for _, scale := range ladder {
		t.Logf("lane=%s scale=%d seeding…", lane, scale)
		point, refuse, err := measureSelectorScalePoint(t, ctx, store, pool, profile, lane, scale, out.SQLTextDigest, firstPoint, out.IndependentVariable)
		if err != nil {
			t.Logf("lane=%s scale=%d REFUSED: %v", lane, scale, err)
			refusals = append(refusals, selectorScaleRefusal{
				Lane: lane, Scale: scale, Reason: "seed_or_measure_failed",
				LargestCompleted: lastCompleted, RowsAtLargest: lastRows,
				BytesAtLargest: lastBytes, WallSecondsLargest: lastWall,
				ObservedError: err.Error(),
			})
			// Stop climbing this lane; larger points would only fail harder.
			break
		}
		if refuse != nil {
			refusals = append(refusals, *refuse)
			break
		}
		out.Points = append(out.Points, point)
		firstPoint = false
		lastCompleted = scale
		lastRows = sumRowCounts(point.RowCounts)
		lastBytes = point.DBBytesApprox
		lastWall = point.SeedWallSeconds + point.MeasureWallSeconds
		if prevShape != "" && point.PlanShape != "" && point.PlanShape != prevShape {
			flip := selectorScalePlanFlip{
				FromScale: prevScale, ToScale: scale,
				FromShape: prevShape, ToShape: point.PlanShape,
				Note: "PLANNER SHAPE FLIP — most important result if present; curve at these scales is not one plan",
			}
			out.PlannerShapeFlips = append(out.PlannerShapeFlips, flip)
			t.Logf("PLANNER SHAPE FLIP lane=%s %d→%d: %q → %q", lane, prevScale, scale, prevShape, point.PlanShape)
		}
		if point.PlanShape != "" {
			prevShape = point.PlanShape
			prevScale = scale
		}
	}
	return out, refusals
}

func measureSelectorScalePoint(
	t *testing.T, ctx context.Context, store *Store, pool *pgxpool.Pool,
	profile VLLMRuntimeProfile, lane string, scale int, sqlDigest string,
	isLaneFirstTimedPoint bool, independentVariableName string,
) (selectorScalePoint, *selectorScaleRefusal, error) {
	t.Helper()
	point := selectorScalePoint{
		Scale:                    scale,
		IndependentVariableValue: scale,
		IndependentVariableName:  independentVariableName,
		TargetMS:                 selectorScaleTargetsMS[scale],
		TargetMetJudgement:       "NOT_JUDGED_BY_HARNESS",
		Seed:                     selectorScaleSeed,
		SQLTextDigest:            sqlDigest,
		Status:                   "measured",
		WallMS:                   map[string]selectorScaleLatency{},
	}

	// Fresh tables for this scale point (isolated DB, but keep WAL lean).
	if err := wipeSelectorScaleTables(ctx, pool); err != nil {
		return point, nil, fmt.Errorf("wipe: %w", err)
	}

	seedStart := time.Now()
	var seedErr error
	var claimer WorkerAuth
	var buyerID uuid.UUID
	var region string
	var jobBacklog int

	switch lane {
	case "batch":
		// free_credit_usd is NUMERIC(12,6) — max 999999.999999; do not overflow.
		buyerID, seedErr = store.CreateBuyerAccount(ctx, fmt.Sprintf("sel-b-%d-%s@example.test", scale, uuid.NewString()[:8]),
			"integration-password", 100_000)
		if seedErr != nil {
			return point, nil, seedErr
		}
		jobBacklog = selectorScaleJobBacklog
		// Enough queued tasks for concurrent samples without claim starvation.
		// Warm-up calls also consume tasks; size the backlog for warm+timed.
		warmBudget := selectorScaleWarmup + selectorScaleLaneFirstPointWarmup
		taskN := selectorMaxInt(jobBacklog, (selectorScaleSamples+warmBudget)*selectorScaleConcurrencyHigh*2)
		var fleet selectorBatchFleet
		fleet, seedErr = seedSelectorBatchFleet(ctx, pool, scale, selectorScaleSeed, taskN, buyerID)
		claimer = WorkerAuth{WorkerID: fleet.ClaimerWorkerID, SupplierID: fleet.ClaimerSupplierID}
		point.Notes = fmt.Sprintf("fixed job/task backlog=%d; independent variable is fleet_workers_online=%d (x-axis unit=workers_online)", taskN, scale)
	case "realtime":
		buyerID, seedErr = store.CreateBuyerAccount(ctx, fmt.Sprintf("sel-r-%d-%s@example.test", scale, uuid.NewString()[:8]),
			"integration-password", 100_000)
		if seedErr != nil {
			return point, nil, seedErr
		}
		seedErr = seedSelectorRealtimeBook(ctx, pool, profile, scale, selectorScaleSeed)
		point.Notes = fmt.Sprintf("x-axis=offer_book_rows_on_profile; seeded %d realtime_worker_offers on profile %s (sha scoped); NOT ambient fleet size", scale, profile.RuntimeProfileID)
	case "service_lease":
		buyerID = uuid.New()
		if _, seedErr = pool.Exec(ctx, `INSERT INTO buyers (id,email) VALUES ($1,$2)`,
			buyerID, fmt.Sprintf("%s@sel-lease-%d.invalid", buyerID, scale)); seedErr != nil {
			return point, nil, seedErr
		}
		if seedErr = store.SeedPrepaidBalance(ctx, buyerID, 500_000_000_000, fmt.Sprintf("sel-lease-%d-%s", scale, buyerID)); seedErr != nil {
			return point, nil, seedErr
		}
		region = "ca-sel-scale"
		seedErr = seedSelectorLeaseBook(ctx, pool, profile, region, scale, selectorScaleSeed)
		point.Notes = fmt.Sprintf("x-axis=offer_book_rows_on_profile_region; seeded %d service_lease_worker_offers on profile %s region %s; NOT ambient fleet size", scale, profile.RuntimeProfileID, region)
	}
	point.SeedWallSeconds = time.Since(seedStart).Seconds()
	if seedErr != nil {
		// Resource refusal: capture what we can.
		rows, _ := tableRowCounts(ctx, pool)
		bytes, _ := approxDBBytes(ctx, pool)
		return point, &selectorScaleRefusal{
			Lane: lane, Scale: scale, Reason: "seed_failed",
			ObservedError: seedErr.Error(), RowsAtLargest: sumRowCounts(rows), BytesAtLargest: bytes,
		}, seedErr
	}

	if _, err := pool.Exec(ctx, `ANALYZE`); err != nil {
		t.Logf("ANALYZE: %v", err)
	}
	// Keep liveness windows fresh after long seeds.
	if err := refreshSelectorScaleLiveness(ctx, pool, profile, region); err != nil {
		return point, nil, fmt.Errorf("refresh liveness: %w", err)
	}

	rows, err := tableRowCounts(ctx, pool)
	if err != nil {
		return point, nil, err
	}
	point.RowCounts = rows
	point.DBBytesApprox, _ = approxDBBytes(ctx, pool)
	point.CandidateStages = selectorCandidateStagesFixed(ctx, pool, lane, profile, region, claimer)

	// EXPLAIN the production selection SQL (not a stand-in).
	planShape, planNodes, explainSummary, buffersSummary, explainRaw, err := explainSelectorSQL(ctx, pool, store, lane, profile, region, claimer, buyerID)
	if err != nil {
		t.Logf("EXPLAIN failed (continuing with wall times): %v", err)
		point.PlanShape = "EXPLAIN_FAILED"
		point.Notes += "; explain_error=" + err.Error()
	} else {
		point.PlanShape = planShape
		point.PlanShapeDetail = planNodes
		point.ExplainAnalyzeSummary = explainSummary
		point.ExplainBuffersSummary = buffersSummary
		point.ExplainRawTruncated = truncateString(explainRaw, 8000)
	}

	loadDuring, _ := readLoadAverage()
	point.LoadAverageDuring = loadDuring

	warmEv := &selectorScaleWarmupEvidence{
		Policy:                         "discard leading production-SQL calls per concurrency cell; record discarded latencies here",
		IsLaneFirstTimedPoint:          isLaneFirstTimedPoint,
		PerCellWarmupCalls:             selectorScaleWarmup,
		LaneFirstPointExtraWarmupCalls: 0,
		DiscardedByCell:                map[string]selectorScaleLatency{},
		ColdestDiscardedMSByCell:       map[string]float64{},
		FirstTimedSampleMSByCell:       map[string]float64{},
		SampleFailuresInWarmup:         map[string]int{},
		SampleFailuresInTimed:          map[string]int{},
		HeartbeatSweepsByCell:          map[string]int{},
		HeartbeatMeanMSByCell:          map[string]float64{},
		HeartbeatSlowestMSByCell:       map[string]float64{},
		Note:                           "Timed wall_ms excludes every discarded warm-up call. Coldest discarded vs first timed sample shows whether plan/cache cold-start was removed from the percentile set.",
	}
	if isLaneFirstTimedPoint {
		warmEv.LaneFirstPointExtraWarmupCalls = selectorScaleLaneFirstPointWarmup
		warmEv.Note += fmt.Sprintf(" Lane-first point also discarded %d extra c=1 warm-up calls before any timed percentile.", selectorScaleLaneFirstPointWarmup)
	}

	measureStart := time.Now()
	for _, conc := range []int{1, selectorScaleConcurrencyHigh} {
		// Replenish capacity before each concurrency cell.
		if err := replenishSelectorScaleCapacity(ctx, pool, store, lane, profile, region, claimer, buyerID); err != nil {
			return point, nil, fmt.Errorf("replenish c=%d: %w", conc, err)
		}
		if err := refreshSelectorScaleLiveness(ctx, pool, profile, region); err != nil {
			return point, nil, err
		}
		nSamples := selectorScaleSampleCount(scale, conc)
		// Lane-first extra warm-up only on the serial cell — that is where the
		// first-point cold plan compile was observed (batch n=1000 p50 ~1.6s).
		extraWarm := 0
		if isLaneFirstTimedPoint && conc == 1 {
			extraWarm = selectorScaleLaneFirstPointWarmup
		}
		result := measureSelectorScaleSamples(t, ctx, store, pool, lane, profile, region, claimer, buyerID, conc, nSamples, selectorScaleWarmup+extraWarm)
		key := fmt.Sprintf("concurrency_%d", conc)
		// Percentiles are SUCCESS-only. Failed calls (funding exhaustion, no
		// work, etc.) stay in the failure counts; including them deflated p50
		// into single-digit ms while the book grew (prior non-monotonic realtime).
		successTimed := filterSuccessDurations(result.Timed, result.TimedFailFlags)
		successWarm := filterSuccessDurations(result.Warmup, result.WarmupFailFlags)
		if len(successTimed) == 0 {
			// All failed: still record failure latencies so the point is not silent,
			// but mark the cell so it cannot be quoted as selection cost.
			point.WallMS[key] = latencyFromDurations(result.Timed)
			point.Notes += fmt.Sprintf("; ALL_FAILED_c%d n=%d (wall_ms is failure latency, not selection cost)", conc, len(result.Timed))
			point.Defects = append(point.Defects, selectorScaleDefect{
				Lane: lane, Scale: scale, Cell: key, Kind: "all_samples_failed",
				Summary: fmt.Sprintf("%s %s scale=%d: every timed sample failed (%s)", lane, key, scale, firstErrorText(result)),
				Evidence: fmt.Sprintf("timed_failures=%d timed_n=%d; wall_ms reflects error-path latency only; errors=%v",
					result.TimedFailures, len(result.Timed), result.DistinctErrors),
			})
		} else {
			point.WallMS[key] = latencyFromDurations(successTimed)
			if len(successTimed) < len(result.Timed) {
				point.Notes += fmt.Sprintf("; success_only_c%d n=%d/%d", conc, len(successTimed), len(result.Timed))
			}
		}
		warmEv.DiscardedByCell[key] = latencyFromDurations(result.Warmup)
		if len(successWarm) > 0 {
			// Prefer success warm max for cold-start proof when available.
			warmEv.ColdestDiscardedMSByCell[key] = float64(maxDuration(result.Warmup)) / float64(time.Millisecond)
		} else if len(result.Warmup) > 0 {
			warmEv.ColdestDiscardedMSByCell[key] = float64(maxDuration(result.Warmup)) / float64(time.Millisecond)
		}
		if len(successTimed) > 0 {
			warmEv.FirstTimedSampleMSByCell[key] = float64(successTimed[0]) / float64(time.Millisecond)
		} else if len(result.Timed) > 0 {
			warmEv.FirstTimedSampleMSByCell[key] = float64(result.Timed[0]) / float64(time.Millisecond)
		}
		warmEv.SampleFailuresInWarmup[key] = result.WarmupFailures
		warmEv.SampleFailuresInTimed[key] = result.TimedFailures
		if result.HeartbeatSweeps > 0 {
			warmEv.HeartbeatSweepsByCell[key] = result.HeartbeatSweeps
			warmEv.HeartbeatMeanMSByCell[key] = result.HeartbeatMeanMS
			warmEv.HeartbeatSlowestMSByCell[key] = result.HeartbeatSlowestMS
			// A sweep that outlasts the window cannot keep the book alive, so
			// any measurement over that book is bounded by the harness, not by
			// the selector. Say so rather than publish the number quietly.
			if result.HeartbeatSlowestMS > float64(selectorScaleLivenessWindow/time.Millisecond) {
				point.Defects = append(point.Defects, selectorScaleDefect{
					Lane: lane, Scale: scale, Cell: key, Kind: "heartbeat_slower_than_liveness_window",
					Summary: fmt.Sprintf("%s %s scale=%d: a supplier-heartbeat sweep took %.0fms against a %s liveness window",
						lane, key, scale, result.HeartbeatSlowestMS, selectorScaleLivenessWindow),
					Evidence: fmt.Sprintf("sweeps=%d mean=%.0fms slowest=%.0fms; the harness cannot hold a book this large alive at the production cadence on this host, so selection cost at this scale is harness-bounded",
						result.HeartbeatSweeps, result.HeartbeatMeanMS, result.HeartbeatSlowestMS),
				})
			}
		}
		if nSamples < selectorScaleSamples {
			point.Notes += fmt.Sprintf("; samples_c%d=%d (reduced at this scale for wall budget; p99 less stable)", conc, nSamples)
		}
		if result.WarmupFailures+result.TimedFailures > 0 {
			point.Notes += fmt.Sprintf("; failures_c%d warm=%d timed=%d", conc, result.WarmupFailures, result.TimedFailures)
		}
		// Contention / lock-convoy defect signature: concurrent cell where the
		// bulk of samples sit near max while min is orders of magnitude smaller,
		// or absolute multi-minute p50 under multi-poller load.
		if conc > 1 {
			lat := point.WallMS[key]
			if defect := detectSelectorScaleContentionDefect(lane, scale, key, lat, result); defect != nil {
				point.Defects = append(point.Defects, *defect)
				point.Notes += "; DEFECT recorded for " + key + " (see point.defects)"
			}
		}
		t.Logf("lane=%s scale=%d c=%d warm_n=%d warm_max=%.3fms timed p50=%.3fms p95=%.3fms p99=%.3fms n=%d fail_w=%d fail_t=%d",
			lane, scale, conc, len(result.Warmup),
			warmEv.ColdestDiscardedMSByCell[key],
			point.WallMS[key].P50, point.WallMS[key].P95, point.WallMS[key].P99, point.WallMS[key].N,
			result.WarmupFailures, result.TimedFailures)
	}
	point.Warmup = warmEv
	point.MeasureWallSeconds = time.Since(measureStart).Seconds()
	return point, nil, nil
}

func maxDuration(ds []time.Duration) time.Duration {
	var m time.Duration
	for _, d := range ds {
		if d > m {
			m = d
		}
	}
	return m
}

// detectSelectorScaleContentionDefect flags multi-poller cells that are not
// single-selection cost measurements (lock convoy / connection starvation).
func detectSelectorScaleContentionDefect(lane string, scale int, cell string, lat selectorScaleLatency, result selectorScaleSampleResult) *selectorScaleDefect {
	if lat.N < 1 || lat.Min <= 0 {
		return nil
	}
	ratio := lat.P50 / lat.Min
	// Multi-minute p50 under concurrency, or p50 >> min with a near-max cluster.
	multiMinute := lat.P50 >= 60_000 // 60s
	spreadConvoy := ratio >= 50 && lat.P50 >= 10_000 && lat.Max > 0 && lat.P50/lat.Max >= 0.9
	if !multiMinute && !spreadConvoy {
		return nil
	}
	kind := "multi_poller_contention"
	summary := fmt.Sprintf("%s %s at scale=%d is a contention result, not single-selection cost", lane, cell, scale)
	evidence := fmt.Sprintf("p50=%.3fms p95=%.3fms min=%.3fms max=%.3fms n=%d p50/min=%.1f timed_failures=%d; concurrent ClaimTasksTx/Authorize/CreateServiceLease under large synthetic state — FOR UPDATE + heavy SQL can convoy; do not quote p50 as selector latency",
		lat.P50, lat.P95, lat.Min, lat.Max, lat.N, ratio, result.TimedFailures)
	if lane == "batch" && scale >= 1_000_000 {
		kind = "batch_1m_concurrency_lock_convoy"
		summary = "batch 1M-worker concurrency cell: multi-poller claim under fleet-relative EXISTS — lock/buffer convoy, not per-claim selection cost"
		evidence += "; independent variable (fleet EXISTS cheaper_class/ask) is evaluated inside each concurrent claim; workers take FOR UPDATE on distinct claimer rows then contend on tasks SKIP LOCKED and shared buffers while scanning a 1M-row fleet per eligible job"
	}
	return &selectorScaleDefect{
		Lane: lane, Scale: scale, Cell: cell, Kind: kind,
		Summary: summary, Evidence: evidence,
	}
}

// selectorScaleSampleCount keeps p50/p95 honest at small scales and reduces
// sample count only where concurrent claim under large fleets would otherwise
// spend hours on host contention (observed 100k c=8 p95 ~7 minutes per sample).
func selectorScaleSampleCount(scale, concurrency int) int {
	switch {
	case scale >= 1_000_000 && concurrency > 1:
		return 5
	case scale >= 1_000_000:
		return 10
	case scale >= 100_000 && concurrency > 1:
		return 10
	case scale >= 100_000:
		return 20
	case scale >= 10_000 && concurrency > 1:
		return 15
	default:
		return selectorScaleSamples
	}
}

type selectorScaleSampleResult struct {
	Warmup         []time.Duration
	Timed          []time.Duration
	WarmupFailures int
	TimedFailures  int
	// Fail flags aligned with Warmup/Timed slices (true = production call errored / no work).
	WarmupFailFlags []bool
	TimedFailFlags  []bool
	// FirstError and DistinctErrors name what refused, so a wholly-failed cell
	// is a diagnosis rather than only an alarm.
	FirstError     *string
	DistinctErrors []string
	// Heartbeat cost during this cell. A sweep that does not fit inside the
	// liveness window is a measurement limit worth publishing.
	HeartbeatSweeps    int
	HeartbeatMeanMS    float64
	HeartbeatSlowestMS float64
}

func measureSelectorScaleSamples(
	t *testing.T, ctx context.Context, store *Store, pool *pgxpool.Pool,
	lane string, profile VLLMRuntimeProfile, region string,
	claimer WorkerAuth, buyerID uuid.UUID, concurrency, nSamples, warmupN int,
) selectorScaleSampleResult {
	t.Helper()
	if nSamples < 1 {
		nSamples = selectorScaleSamples
	}
	if warmupN < 0 {
		warmupN = 0
	}
	total := warmupN + nSamples
	raw := make([]time.Duration, total)
	failFlags := make([]bool, total)
	var fail atomic.Int32

	// A cell that reports "every sample failed" without saying WHY is half a
	// finding. The first run's realtime cells at 100k and 1M read 8.8ms and
	// 17.9ms p50 — under target — because a refusal is fast; the defect record
	// caught that they were refusals but could not say what refused, so the
	// diagnosis cost a second run. Record the error text with the count.
	var firstErr atomic.Pointer[string]
	var errKinds sync.Map
	noteErr := func(err error) {
		msg := err.Error()
		firstErr.CompareAndSwap(nil, &msg)
		n, _ := errKinds.LoadOrStore(msg, new(int64))
		atomic.AddInt64(n.(*int64), 1)
	}

	var maxUSD, estUSD float64
	var maxPrompt, maxCompletion int64
	const estPrompt, estCompletion int64 = 7, 2
	if lane == "realtime" {
		maxUSD, estUSD, maxPrompt, maxCompletion = realtimeAuthCeiling(t, profile, estPrompt, estCompletion)
	}

	// Concurrent buyers for lease/realtime when c>1 so funding locks do not
	// serialise the entire sample set on one buyer row.
	buyers := []uuid.UUID{buyerID}
	if concurrency > 1 && (lane == "realtime" || lane == "service_lease") {
		for i := 1; i < concurrency; i++ {
			var b uuid.UUID
			var err error
			switch lane {
			case "realtime":
				b, err = store.CreateBuyerAccount(ctx, fmt.Sprintf("sel-c-%d-%s@example.test", i, uuid.NewString()[:8]),
					"integration-password", 100_000)
			case "service_lease":
				b = uuid.New()
				_, err = pool.Exec(ctx, `INSERT INTO buyers (id,email) VALUES ($1,$2)`,
					b, fmt.Sprintf("%s@sel-c-%d.invalid", b, i))
				if err == nil {
					err = store.SeedPrepaidBalance(ctx, b, 500_000_000_000, "sel-c-"+b.String())
				}
			}
			if err != nil {
				t.Fatalf("extra buyer: %v", err)
			}
			buyers = append(buyers, b)
		}
	}

	runOne := func(workerIdx int, idx int) {
		b := buyers[workerIdx%len(buyers)]
		start := time.Now()
		var callErr error
		switch lane {
		case "batch":
			// Ensure a claimable task exists for this sample.
			if err := ensureQueuedBatchTask(ctx, pool, b); err != nil {
				fail.Add(1)
				failFlags[idx] = true
				noteErr(err)
				raw[idx] = time.Since(start)
				return
			}
			// Each concurrent goroutine claims as a DIFFERENT fleet worker.
			// ClaimTasksTx takes FOR UPDATE on the claiming worker row first;
			// reusing one claimer serialises the whole cell and measures the
			// wrong contention (worker lock, not task SKIP LOCKED).
			auth := claimer
			if concurrency > 1 {
				auth = WorkerAuth{
					WorkerID:   detUUID(selectorScaleSeed, "wrk", workerIdx),
					SupplierID: detUUID(selectorScaleSeed, "sup", workerIdx),
				}
			}
			got, err := store.ClaimTasksTx(ctx, auth)
			callErr = err
			if err == nil && got == nil {
				callErr = errors.New("ClaimTasksTx returned no work")
			}
		case "realtime":
			// Authorize then immediately terminalise so reserved free_credit /
			// prepaid does not accumulate across warm+timed samples and turn
			// later points into fast funding failures (observed: 100k timed
			// fail_t=n with p50~6ms). Teardown is harness accounting, not a
			// production-SQL change.
			var contract RealtimeContract
			contract, _, callErr = store.AuthorizeRealtimeContract(ctx, RealtimeContractAuthorization{
				RequestID: "sel-" + uuid.NewString(), BuyerID: b, Profile: profile,
				InputCommitment: strings.Repeat("a", 64), RequestSHA256: strings.Repeat("b", 64),
				MaximumPriceUSD: maxUSD, EstimatedPriceUSD: estUSD, DeadlineAt: time.Now().Add(time.Minute),
				MaximumPromptTokens: maxPrompt, MaximumCompletionTokens: maxCompletion,
				EstimatedPromptTokens: estPrompt, EstimatedCompletionTokens: estCompletion,
				BuyerDeclaredCeilingUSD: maxUSD * 1.1,
			})
			if callErr == nil && contract.ID != uuid.Nil {
				_, _ = store.FinalizeRealtimeFailure(ctx, contract.ID, uuid.New(), 500, 1,
					"selector_scale", "harness sample teardown", false)
			}
		case "service_lease":
			var lease ServiceLease
			lease, callErr = store.CreateServiceLease(ctx, b, ServiceLeaseRequest{
				RuntimeProfileID: profile.RuntimeProfileID, Region: region, Currency: "usd",
				MinimumReplicas: 1, MaximumReplicas: 1, TermSeconds: 60,
				MaximumP95LatencyMilliseconds: 500, BuyerDeclaredCeilingNanos: 135_000_000,
			})
			// Capacity is restored by replenishSelectorScaleCapacity between
			// cells; terminalising here keeps open reservations from stacking
			// inside a cell when many samples share one buyer.
			if callErr == nil && lease.ID != uuid.Nil {
				_, _ = pool.Exec(ctx, `
					UPDATE service_leases
					   SET state='CANCELLED', finalized_at=COALESCE(finalized_at, now()),
					       expires_at=LEAST(expires_at, now())
					 WHERE id=$1 AND state IN ('ACTIVE','UPGRADING','FAILOVER_REQUIRED')`, lease.ID)
				_, _ = pool.Exec(ctx, `
					UPDATE service_lease_worker_offers
					   SET available_warm_replicas=maximum_warm_replicas, last_seen_at=now(), status='READY'
					 WHERE worker_id=$1`, lease.WorkerID)
			}
		}
		raw[idx] = time.Since(start)
		if callErr != nil {
			fail.Add(1)
			failFlags[idx] = true
			noteErr(callErr)
			// Do not t.Error from workers (racy); count only.
		}
	}

	// A real supply book does not sit still. Suppliers re-register every 15s
	// (agent/src/vllm.rs HEARTBEAT_INTERVAL) against the 45s liveness window in
	// realtime_store.go:1099, so in production the book is continuously alive.
	//
	// A harness that seeds once and then measures has no heartbeat, and at 100k
	// one authorization takes ~14s — three consecutive calls outlive the window
	// and the book goes dark underneath the scan. That produced `no active
	// compatible realtime supply` on every sample at 100k and 1M, timed at
	// 8.8ms and 17.9ms, which are the two fastest numbers in the artifact and
	// mean nothing. Refusal is fast.
	//
	// So the heartbeat runs at the production cadence for the lanes that have a
	// liveness window. It contends with the selection SQL exactly as a real
	// supplier fleet's registrations would; its own cost is measured rather
	// than assumed, because at a large book the sweep may not fit in the
	// window, and that would be a finding rather than something to hide.
	var beats atomic.Int64
	var beatNanos atomic.Int64
	var beatSlowest atomic.Int64
	stopBeat := make(chan struct{})
	var beatWG sync.WaitGroup
	if lane == "realtime" || lane == "service_lease" {
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
					if err := refreshSelectorScaleLiveness(ctx, pool, profile, region); err != nil {
						continue
					}
					took := time.Since(start).Nanoseconds()
					beats.Add(1)
					beatNanos.Add(took)
					for {
						prev := beatSlowest.Load()
						if took <= prev || beatSlowest.CompareAndSwap(prev, took) {
							break
						}
					}
				}
			}
		}()
	}
	defer func() {
		close(stopBeat)
		beatWG.Wait()
	}()

	// Phase 1: serial warm-up (always concurrency=1 shape) so plan compile and
	// cold shared-buffers land in the discarded set, not the timed percentiles.
	// Even for c=8 timed cells we warm serially first — cold-start is not the
	// contention signal under test.
	for i := 0; i < warmupN; i++ {
		runOne(0, i)
	}

	// Phase 2: timed samples at the requested concurrency.
	jobs := make(chan int)
	var wg sync.WaitGroup
	for w := 0; w < concurrency; w++ {
		wg.Add(1)
		go func(workerIdx int) {
			defer wg.Done()
			for idx := range jobs {
				runOne(workerIdx, idx)
			}
		}(w)
	}
	for i := warmupN; i < total; i++ {
		jobs <- i
	}
	close(jobs)
	wg.Wait()

	if n := fail.Load(); n > 0 {
		t.Logf("lane=%s c=%d sample failures=%d/%d (warm=%d timed=%d; failures stay in their bucket)",
			lane, concurrency, n, total, countTrue(failFlags[:warmupN]), countTrue(failFlags[warmupN:]))
	}
	out := selectorScaleSampleResult{
		Warmup:          append([]time.Duration(nil), raw[:warmupN]...),
		Timed:           append([]time.Duration(nil), raw[warmupN:]...),
		WarmupFailFlags: append([]bool(nil), failFlags[:warmupN]...),
		TimedFailFlags:  append([]bool(nil), failFlags[warmupN:]...),
	}
	out.WarmupFailures = countTrue(out.WarmupFailFlags)
	out.TimedFailures = countTrue(out.TimedFailFlags)
	out.FirstError, out.DistinctErrors = firstErr.Load(), distinctErrorList(&errKinds)
	if n := beats.Load(); n > 0 {
		out.HeartbeatSweeps = int(n)
		out.HeartbeatMeanMS = float64(beatNanos.Load()) / float64(n) / float64(time.Millisecond)
		out.HeartbeatSlowestMS = float64(beatSlowest.Load()) / float64(time.Millisecond)
	}
	return out
}

// distinctErrorList renders the observed error texts, so a cell that failed
// wholly for one reason reads differently from a cell that failed for several.
func distinctErrorList(m *sync.Map) []string {
	var out []string
	m.Range(func(k, v any) bool {
		out = append(out, fmt.Sprintf("%s (x%d)", k.(string), *v.(*int64)))
		return true
	})
	sort.Strings(out)
	return out
}

func countTrue(flags []bool) int {
	n := 0
	for _, f := range flags {
		if f {
			n++
		}
	}
	return n
}

// filterSuccessDurations keeps only samples whose fail flag is false, preserving
// issue order so first-timed-sample remains meaningful.
func filterSuccessDurations(ds []time.Duration, fail []bool) []time.Duration {
	if len(ds) == 0 {
		return nil
	}
	out := make([]time.Duration, 0, len(ds))
	for i, d := range ds {
		if i < len(fail) && fail[i] {
			continue
		}
		out = append(out, d)
	}
	return out
}

// --- fleet generators (CopyFrom, fixed seed) --------------------------------

type selectorBatchFleet struct {
	ClaimerWorkerID   uuid.UUID
	ClaimerSupplierID uuid.UUID
	WorkerCount       int
	TaskCount         int
}

func seedSelectorBatchFleet(ctx context.Context, pool *pgxpool.Pool, workers int, seed int64, tasks int, buyerID uuid.UUID) (selectorBatchFleet, error) {
	if workers < 1 {
		return selectorBatchFleet{}, errors.New("workers must be >= 1")
	}
	profileID, revision, digest, err := candleGovernedIdentity()
	if err != nil {
		return selectorBatchFleet{}, err
	}
	matrix := generatedRuntimeMatrixSHA256
	now := time.Now().UTC()

	// Suppliers
	if err := copyInChunks(ctx, pool, workers, selectorScaleCopyChunk, func(start, n int) error {
		_, err := pool.CopyFrom(ctx, pgx.Identifier{"suppliers"},
			[]string{"id", "email", "status", "reputation", "completed_tasks"},
			pgx.CopyFromSlice(n, func(i int) ([]any, error) {
				idx := start + i
				id := detUUID(seed, "sup", idx)
				return []any{id, fmt.Sprintf("sel-sup-%d-%d@example.test", seed, idx), "active", 0.95, int64(100)}, nil
			}))
		return err
	}); err != nil {
		return selectorBatchFleet{}, fmt.Errorf("copy suppliers: %w", err)
	}

	// Workers (sandboxed, live, claim-eligible identity)
	if err := copyInChunks(ctx, pool, workers, selectorScaleCopyChunk, func(start, n int) error {
		_, err := pool.CopyFrom(ctx, pgx.Identifier{"workers"},
			[]string{"id", "supplier_id", "hw_class", "memory_gb", "effective_memory_gb",
				"last_seen_at", "throttled", "min_payout_usd_hr", "sandboxed", "unsandboxed_opt_in",
				"engine", "build_hash", "build_identity_policy", "hardware_identity",
				"runtime_profile_id", "runtime_profile_revision", "runtime_profile_digest"},
			pgx.CopyFromSlice(n, func(i int) ([]any, error) {
				idx := start + i
				wid := detUUID(seed, "wrk", idx)
				sid := detUUID(seed, "sup", idx)
				return []any{
					wid, sid, "apple_silicon_max", float32(64), float32(64),
					now, false, 0.10, true, false,
					"candle", testOnlyEngineBuildHash, currentEngineBuildIdentityPolicy, testOnlyHardwareIdentity,
					profileID, revision, digest,
				}, nil
			}))
		return err
	}); err != nil {
		return selectorBatchFleet{}, fmt.Errorf("copy workers: %w", err)
	}

	// Capabilities (legacy job path: routable + matching matrix)
	if err := copyInChunks(ctx, pool, workers, selectorScaleCopyChunk, func(start, n int) error {
		_, err := pool.CopyFrom(ctx, pgx.Identifier{"worker_authorized_capabilities"},
			[]string{"worker_id", "cell_id", "runtime_id", "job_type", "model_ref", "model_kind", "matrix_sha256", "authorized_at", "routable"},
			pgx.CopyFromSlice(n, func(i int) ([]any, error) {
				idx := start + i
				wid := detUUID(seed, "wrk", idx)
				return []any{wid, "cell", "rt", "embed", "all-minilm-l6-v2", "hf", matrix, now, true}, nil
			}))
		return err
	}); err != nil {
		return selectorBatchFleet{}, fmt.Errorf("copy capabilities: %w", err)
	}

	// Jobs + tasks backlog (eligible queue for claim)
	if err := copyInChunks(ctx, pool, tasks, selectorScaleCopyChunk, func(start, n int) error {
		_, err := pool.CopyFrom(ctx, pgx.Identifier{"jobs"},
			[]string{"id", "buyer_id", "status", "job_type", "model_ref", "input_ref", "task_count",
				"offered_rate_usd_hr", "min_memory_gb", "tier", "currency"},
			pgx.CopyFromSlice(n, func(i int) ([]any, error) {
				idx := start + i
				jid := detUUID(seed, "job", idx)
				return []any{jid, buyerID, "running", "embed", "all-minilm-l6-v2", "in", 1,
					10.0, float32(0), "batch", "usd"}, nil
			}))
		return err
	}); err != nil {
		return selectorBatchFleet{}, fmt.Errorf("copy jobs: %w", err)
	}
	if err := copyInChunks(ctx, pool, tasks, selectorScaleCopyChunk, func(start, n int) error {
		_, err := pool.CopyFrom(ctx, pgx.Identifier{"tasks"},
			[]string{"id", "job_id", "status", "input_ref", "result_key", "visible_at"},
			pgx.CopyFromSlice(n, func(i int) ([]any, error) {
				idx := start + i
				jid := detUUID(seed, "job", idx)
				tid := detUUID(seed, "tsk", idx)
				return []any{tid, jid, "queued", "in", "rk", now}, nil
			}))
		return err
	}); err != nil {
		return selectorBatchFleet{}, fmt.Errorf("copy tasks: %w", err)
	}

	return selectorBatchFleet{
		ClaimerWorkerID:   detUUID(seed, "wrk", 0),
		ClaimerSupplierID: detUUID(seed, "sup", 0),
		WorkerCount:       workers,
		TaskCount:         tasks,
	}, nil
}

func seedSelectorRealtimeBook(ctx context.Context, pool *pgxpool.Pool, profile VLLMRuntimeProfile, offers int, seed int64) error {
	if offers < 1 {
		return errors.New("offers must be >= 1")
	}
	now := time.Now().UTC()
	// Suppliers + workers
	if err := copyInChunks(ctx, pool, offers, selectorScaleCopyChunk, func(start, n int) error {
		_, err := pool.CopyFrom(ctx, pgx.Identifier{"suppliers"},
			[]string{"id", "email", "status"},
			pgx.CopyFromSlice(n, func(i int) ([]any, error) {
				idx := start + i
				return []any{detUUID(seed, "rts", idx), fmt.Sprintf("sel-rt-sup-%d-%d@example.test", seed, idx), "active"}, nil
			}))
		return err
	}); err != nil {
		return fmt.Errorf("rt suppliers: %w", err)
	}
	if err := copyInChunks(ctx, pool, offers, selectorScaleCopyChunk, func(start, n int) error {
		_, err := pool.CopyFrom(ctx, pgx.Identifier{"workers"},
			[]string{"id", "supplier_id", "hw_class", "last_seen_at"},
			pgx.CopyFromSlice(n, func(i int) ([]any, error) {
				idx := start + i
				return []any{detUUID(seed, "rtw", idx), detUUID(seed, "rts", idx), "nvidia_24gb", now}, nil
			}))
		return err
	}); err != nil {
		return fmt.Errorf("rt workers: %w", err)
	}

	// One placement plan for the whole book (same profile/topology).
	plan, err := newRealtimePlacementPlan(profile, RealtimeOfferRegistration{
		RuntimeProfileID: profile.RuntimeProfileID, RuntimeProfileSHA256: profile.ProfileSHA256,
		HWClass: "nvidia_24gb", GPUCount: 1, MemoryGBPerGPU: 24,
		UpstreamBaseURL: "http://127.0.0.1:8811/v1", UpstreamToken: "cx_vllm_selector_scale_token_12345678",
		Warmth: "HOT", MaxActiveSequences: 1000, AvailableSequences: 1000,
		SupplierInputUSDPerMillionTokens: 0.08, SupplierOutputUSDPerMillionTokens: 0.30,
	})
	if err != nil {
		return fmt.Errorf("placement plan: %w", err)
	}
	placementJSON, placementSHA, err := encodeRealtimePlacementPlan(plan)
	if err != nil {
		return fmt.Errorf("encode placement: %w", err)
	}
	sealed := sealToken("cx_vllm_selector_scale_token_12345678")
	if !strings.HasPrefix(sealed, "enc:") {
		return errors.New("MERC_TOKEN_KEY must seal realtime upstream tokens")
	}

	// Slight ask spread so ORDER BY is non-trivial; all under buyer ceilings.
	if err := copyInChunks(ctx, pool, offers, selectorScaleCopyChunk, func(start, n int) error {
		_, err := pool.CopyFrom(ctx, pgx.Identifier{"realtime_worker_offers"},
			[]string{"worker_id", "supplier_id", "runtime_profile_id", "runtime_profile_sha256",
				"placement_plan", "placement_plan_sha256",
				"upstream_base_url", "upstream_token_sealed", "warmth",
				"max_active_sequences", "available_sequences",
				"supplier_input_usd_per_million_tokens", "supplier_output_usd_per_million_tokens",
				"status", "last_seen_at", "updated_at"},
			pgx.CopyFromSlice(n, func(i int) ([]any, error) {
				idx := start + i
				// Deterministic micro-spread on input rate only.
				inRate := 0.08 + float64(idx%100)*0.0001
				return []any{
					detUUID(seed, "rtw", idx), detUUID(seed, "rts", idx),
					profile.RuntimeProfileID, profile.ProfileSHA256,
					placementJSON, placementSHA,
					"http://127.0.0.1:8811/v1", sealed, "HOT",
					1000, 1000,
					inRate, 0.30,
					"ACTIVE", now, now,
				}, nil
			}))
		return err
	}); err != nil {
		return fmt.Errorf("rt offers: %w", err)
	}
	return nil
}

func seedSelectorLeaseBook(ctx context.Context, pool *pgxpool.Pool, profile VLLMRuntimeProfile, region string, offers int, seed int64) error {
	if offers < 1 {
		return errors.New("offers must be >= 1")
	}
	now := time.Now().UTC()
	if err := copyInChunks(ctx, pool, offers, selectorScaleCopyChunk, func(start, n int) error {
		_, err := pool.CopyFrom(ctx, pgx.Identifier{"suppliers"},
			[]string{"id", "email", "status"},
			pgx.CopyFromSlice(n, func(i int) ([]any, error) {
				idx := start + i
				return []any{detUUID(seed, "lss", idx), fmt.Sprintf("sel-ls-sup-%d-%d@example.test", seed, idx), "active"}, nil
			}))
		return err
	}); err != nil {
		return fmt.Errorf("lease suppliers: %w", err)
	}
	if err := copyInChunks(ctx, pool, offers, selectorScaleCopyChunk, func(start, n int) error {
		_, err := pool.CopyFrom(ctx, pgx.Identifier{"workers"},
			[]string{"id", "supplier_id", "hw_class", "last_seen_at"},
			pgx.CopyFromSlice(n, func(i int) ([]any, error) {
				idx := start + i
				return []any{detUUID(seed, "lsw", idx), detUUID(seed, "lss", idx), "nvidia_24gb", now}, nil
			}))
		return err
	}); err != nil {
		return fmt.Errorf("lease workers: %w", err)
	}
	// Measured warm residency required for READY offers to advertise capacity.
	if err := copyInChunks(ctx, pool, offers, selectorScaleCopyChunk, func(start, n int) error {
		_, err := pool.CopyFrom(ctx, pgx.Identifier{"worker_model_state"},
			[]string{"worker_id", "model_id", "last_seen_warm", "rss_delta_bytes", "load_ms"},
			pgx.CopyFromSlice(n, func(i int) ([]any, error) {
				idx := start + i
				return []any{detUUID(seed, "lsw", idx), profile.ModelAlias, now, int64(100 * 1024 * 1024), int64(1500)}, nil
			}))
		return err
	}); err != nil {
		return fmt.Errorf("lease model state: %w", err)
	}
	if err := copyInChunks(ctx, pool, offers, selectorScaleCopyChunk, func(start, n int) error {
		_, err := pool.CopyFrom(ctx, pgx.Identifier{"service_lease_worker_offers"},
			[]string{"worker_id", "supplier_id", "runtime_profile_id", "runtime_profile_sha256", "region", "currency",
				"maximum_warm_replicas", "available_warm_replicas",
				"supplier_nanos_per_replica_hour", "residency_nanos_per_replica_hour",
				"supports_rolling_upgrade", "p95_latency_milliseconds",
				"latency_measurement_count", "latency_window_seconds", "latency_measurement_kind",
				"status", "last_seen_at", "updated_at"},
			pgx.CopyFromSlice(n, func(i int) ([]any, error) {
				idx := start + i
				// Deterministic ask ladder; all clear a 135ms ceiling.
				ask := int64(2_000_000_000 + int64(idx%1000)*100_000)
				return []any{
					detUUID(seed, "lsw", idx), detUUID(seed, "lss", idx),
					profile.RuntimeProfileID, profile.ProfileSHA256, region, "usd",
					3, 3,
					ask, int64(200_000_000),
					true, int64(200),
					5, 15, "DATA_PLANE_COMPLETIONS_V1",
					"READY", now, now,
				}, nil
			}))
		return err
	}); err != nil {
		return fmt.Errorf("lease offers: %w", err)
	}
	return nil
}

// --- EXPLAIN / candidates / replenish ---------------------------------------

func serviceLeaseBookWalkSQL() string {
	return `
		SELECT worker_id,supplier_id,supplier_nanos_per_replica_hour,
		       residency_nanos_per_replica_hour,available_warm_replicas
		  FROM service_lease_worker_offers
		 WHERE runtime_profile_id=$1 AND runtime_profile_sha256=$2 AND region=$3 AND currency=$6 AND status='READY'
		   AND p95_latency_milliseconds>0 AND latency_measurement_count>=5
		   AND latency_window_seconds BETWEEN 1 AND 300 AND latency_measurement_kind='DATA_PLANE_COMPLETIONS_V1'
		   AND p95_latency_milliseconds <= $5 AND last_seen_at > now()-interval '45 seconds' AND available_warm_replicas >= $4
		 ORDER BY (supplier_nanos_per_replica_hour + residency_nanos_per_replica_hour) ASC,
		          supplier_nanos_per_replica_hour ASC,worker_id ASC
		 FOR UPDATE`
}

func explainSelectorSQL(
	ctx context.Context, pool *pgxpool.Pool, store *Store,
	lane string, profile VLLMRuntimeProfile, region string,
	claimer WorkerAuth, buyerID uuid.UUID,
) (shape string, nodes []string, summary, buffers, raw string, err error) {
	var body string
	var args []any
	switch lane {
	case "batch":
		// EXPLAIN ANALYZE runs the production UPDATE…RETURNING once, then we
		// recycle the claimed task. At 1M workers the instrumented ANALYZE can
		// run for tens of minutes; we bound it and fall back to EXPLAIN (BUFFERS).
		body = ClaimTaskSQL("t.claimed_by IS NULL")
		var hwClass string
		var selfMin float64
		if err := pool.QueryRow(ctx, `SELECT COALESCE(hw_class,''), COALESCE(min_payout_usd_hr,0) FROM workers WHERE id=$1`,
			claimer.WorkerID).Scan(&hwClass, &selfMin); err != nil {
			return "", nil, "", "", "", err
		}
		rank := hwClassCostRank(hwClass)
		args = []any{claimer.WorkerID, 2 /* tier */, rank, generatedRuntimeMatrixSHA256,
			selfMin, askDeferralWindow.String(), SettlementCurrencyCode()}
	case "realtime":
		body = realtimeAuthorizeSelectOfferSQLSkip
		args = []any{profile.RuntimeProfileID, profile.ProfileSHA256,
			profile.BuyerInputUSDPerMillionTokens, profile.BuyerOutputUSDPerMillionTokens,
			minRealtimeOutcomeSamples}
	case "service_lease":
		body = serviceLeaseBookWalkSQL()
		args = []any{profile.RuntimeProfileID, profile.ProfileSHA256, region, 1, 500, "usd"}
	default:
		return "", nil, "", "", "", fmt.Errorf("unknown lane %s", lane)
	}

	runExplain := func(analyze bool) ([]string, error) {
		tx, err := pool.Begin(ctx)
		if err != nil {
			return nil, err
		}
		defer tx.Rollback(ctx)
		// Bound instrumented plans so a 1M-worker ANALYZE cannot hang the ladder.
		if _, err := tx.Exec(ctx, `SET LOCAL statement_timeout = '120s'`); err != nil {
			return nil, err
		}
		prefix := "EXPLAIN (BUFFERS, FORMAT TEXT) "
		if analyze {
			prefix = "EXPLAIN (ANALYZE, BUFFERS, FORMAT TEXT) "
		}
		rows, err := tx.Query(ctx, prefix+body, args...)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var lines []string
		for rows.Next() {
			var line string
			if err := rows.Scan(&line); err != nil {
				return nil, err
			}
			lines = append(lines, line)
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
		// Commit so ANALYZE side effects are visible for recycle; plain EXPLAIN is read-only.
		if err := tx.Commit(ctx); err != nil {
			return nil, err
		}
		return lines, nil
	}

	lines, err := runExplain(true)
	mode := "ANALYZE+BUFFERS"
	if err != nil {
		// Timeout / cancel → plan shape without execution.
		fallback, ferr := runExplain(false)
		if ferr != nil {
			return "", nil, "", "", "", fmt.Errorf("EXPLAIN ANALYZE: %v; EXPLAIN fallback: %w", err, ferr)
		}
		lines = fallback
		mode = "BUFFERS_ONLY (ANALYZE timed out or failed: " + err.Error() + ")"
	}
	raw = strings.Join(lines, "\n")
	shape, nodes = summarizePlanShape(lines)
	summary, buffers = summarizeExplainMeta(lines)
	if summary == "" {
		summary = "explain_mode=" + mode
	} else {
		summary = mode + "; " + summary
	}

	// Undo EXPLAIN ANALYZE side effects that consume capacity / claim tasks.
	// Batch tasks that have crossed queued→running freeze execution identity;
	// never UPDATE those columns back to NULL — delete and re-seed instead.
	switch lane {
	case "batch":
		if err := recycleClaimedBatchTasks(ctx, pool, buyerID); err != nil {
			return shape, nodes, summary, buffers, raw, fmt.Errorf("recycle after explain: %w", err)
		}
	case "realtime":
		_, _ = pool.Exec(ctx, `
			UPDATE realtime_worker_offers
			   SET available_sequences=max_active_sequences, status='ACTIVE', last_seen_at=now()`)
	case "service_lease":
		// ANALYZE with FOR UPDATE does not decrement; no capacity change.
		_ = store
		_ = buyerID
	}
	return shape, nodes, summary, buffers, raw, nil
}

// recycleClaimedBatchTasks removes claimed/running fixture tasks (execution
// identity is immutable outside claim) and inserts fresh queued work so the
// next ClaimTasksTx sample has supply.
func recycleClaimedBatchTasks(ctx context.Context, pool *pgxpool.Pool, buyerID uuid.UUID) error {
	if _, err := pool.Exec(ctx, `
		DELETE FROM task_execution_history
		 WHERE task_id IN (SELECT id FROM tasks WHERE claimed_by IS NOT NULL OR status <> 'queued')`); err != nil {
		return err
	}
	if _, err := pool.Exec(ctx, `
		DELETE FROM tasks WHERE claimed_by IS NOT NULL OR status <> 'queued'`); err != nil {
		return err
	}
	// Drop orphan jobs that no longer have tasks.
	if _, err := pool.Exec(ctx, `
		DELETE FROM jobs j WHERE NOT EXISTS (SELECT 1 FROM tasks t WHERE t.job_id=j.id)`); err != nil {
		return err
	}
	var queued int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM tasks WHERE status='queued' AND claimed_by IS NULL`).Scan(&queued); err != nil {
		return err
	}
	// Cover timed samples + warm-up + concurrent claimers; keep a 2× cushion.
	warmBudget := selectorScaleWarmup + selectorScaleLaneFirstPointWarmup
	need := (selectorScaleSamples+warmBudget)*selectorScaleConcurrencyHigh*2 - queued
	if need < 1 {
		return nil
	}
	for i := 0; i < need; i++ {
		if err := ensureQueuedBatchTask(ctx, pool, buyerID); err != nil {
			return err
		}
	}
	return nil
}

func summarizePlanShape(lines []string) (string, []string) {
	var nodes []string
	for _, line := range lines {
		trim := strings.TrimSpace(line)
		// EXPLAIN TEXT: "Seq Scan on workers w2" / "Index Scan using ..." / "LockRows"
		for _, prefix := range []string{
			"Seq Scan", "Index Scan", "Index Only Scan", "Bitmap Heap Scan", "Bitmap Index Scan",
			"Nested Loop", "Hash Join", "Merge Join", "CTE Scan", "Subquery Scan", "Materialize",
			"Sort", "LockRows", "Limit", "Update", "Result", "Append", "Gather", "Gather Merge",
			"Parallel Seq Scan", "Parallel Index Scan", "Hash", "Aggregate", "GroupAggregate",
			"Incremental Sort", "Memoize",
		} {
			if strings.HasPrefix(trim, prefix) || strings.Contains(trim, "->  "+prefix) {
				// Normalize: strip arrow and cost/actual tails for the shape key.
				node := prefix
				// Prefer the full leading token after "->  "
				if i := strings.Index(trim, "->  "); i >= 0 {
					rest := trim[i+4:]
					if sp := strings.IndexAny(rest, " ("); sp > 0 {
						nodes = append(nodes, rest[:sp])
					} else {
						nodes = append(nodes, strings.Fields(rest)[0])
					}
				} else {
					nodes = append(nodes, node)
				}
				break
			}
		}
	}
	if len(nodes) == 0 {
		return "UNPARSED", nil
	}
	// Compact shape: unique ordered node types joined.
	seen := map[string]bool{}
	var uniq []string
	for _, n := range nodes {
		if !seen[n] {
			seen[n] = true
			uniq = append(uniq, n)
		}
	}
	return strings.Join(uniq, " > "), uniq
}

func summarizeExplainMeta(lines []string) (summary, buffers string) {
	var exec, planning, sharedHit, sharedRead string
	for _, line := range lines {
		l := strings.TrimSpace(line)
		if strings.HasPrefix(l, "Execution Time:") {
			exec = l
		}
		if strings.HasPrefix(l, "Planning Time:") {
			planning = l
		}
		if strings.Contains(l, "Buffers:") {
			buffers = l
			// Extract hit/read tokens when present.
			if strings.Contains(l, "shared hit=") {
				sharedHit = l
			}
			if strings.Contains(l, "shared read=") {
				sharedRead = l
			}
		}
	}
	summary = strings.TrimSpace(strings.Join([]string{planning, exec}, "; "))
	if buffers == "" {
		buffers = strings.TrimSpace(sharedHit + " " + sharedRead)
	}
	return summary, buffers
}

func scanCount(ctx context.Context, pool *pgxpool.Pool, dest map[string]int64, key, q string, args ...any) {
	var n int64
	if err := pool.QueryRow(ctx, q, args...).Scan(&n); err == nil {
		dest[key] = n
	}
}

func selectorCandidateStagesFixed(ctx context.Context, pool *pgxpool.Pool, lane string, profile VLLMRuntimeProfile, region string, claimer WorkerAuth) map[string]int64 {
	out := map[string]int64{}
	switch lane {
	case "batch":
		scanCount(ctx, pool, out, "workers_online_60s",
			`SELECT count(*) FROM workers WHERE last_seen_at > now()-interval '60 seconds'`)
		scanCount(ctx, pool, out, "tasks_queued",
			`SELECT count(*) FROM tasks WHERE status IN ('queued','retrying')`)
		scanCount(ctx, pool, out, "jobs_active",
			`SELECT count(*) FROM jobs WHERE status NOT IN ('cancelled','failed','complete')`)
		scanCount(ctx, pool, out, "capabilities",
			`SELECT count(*) FROM worker_authorized_capabilities`)
		if claimer.WorkerID != uuid.Nil {
			scanCount(ctx, pool, out, "tasks_capability_eligible_for_claimer", `
				SELECT count(*) FROM tasks t
				  JOIN jobs j ON j.id=t.job_id
				 WHERE t.status IN ('queued','retrying')
				   AND COALESCE(t.visible_at,t.created_at) <= now()
				   AND t.claimed_by IS NULL
				   AND EXISTS (
				     SELECT 1 FROM worker_authorized_capabilities wac
				      WHERE wac.worker_id=$1 AND wac.job_type=j.job_type
				        AND wac.model_ref=COALESCE(j.model_ref,'')
				        AND wac.matrix_sha256=$2 AND wac.routable
				   )`, claimer.WorkerID, generatedRuntimeMatrixSHA256)
		}
	case "realtime":
		scanCount(ctx, pool, out, "active_offers_live", `
			SELECT count(*) FROM realtime_worker_offers o
			 JOIN suppliers s ON s.id=o.supplier_id
			 WHERE o.runtime_profile_id=$1 AND o.runtime_profile_sha256=$2
			   AND o.status='ACTIVE' AND o.available_sequences>0
			   AND o.last_seen_at > now()-interval '45 seconds'
			   AND s.status='active' AND s.quarantined_at IS NULL`,
			profile.RuntimeProfileID, profile.ProfileSHA256)
		scanCount(ctx, pool, out, "offers_on_profile",
			`SELECT count(*) FROM realtime_worker_offers WHERE runtime_profile_id=$1`,
			profile.RuntimeProfileID)
	case "service_lease":
		scanCount(ctx, pool, out, "ready_offers_live", `
			SELECT count(*) FROM service_lease_worker_offers
			 WHERE runtime_profile_id=$1 AND region=$2 AND status='READY'
			   AND available_warm_replicas >= 1
			   AND last_seen_at > now()-interval '45 seconds'`,
			profile.RuntimeProfileID, region)
		scanCount(ctx, pool, out, "offers_on_profile_region",
			`SELECT count(*) FROM service_lease_worker_offers WHERE runtime_profile_id=$1 AND region=$2`,
			profile.RuntimeProfileID, region)
	}
	return out
}

func replenishSelectorScaleCapacity(
	ctx context.Context, pool *pgxpool.Pool, store *Store,
	lane string, profile VLLMRuntimeProfile, region string,
	claimer WorkerAuth, buyerID uuid.UUID,
) error {
	switch lane {
	case "batch":
		return recycleClaimedBatchTasks(ctx, pool, buyerID)
	case "realtime":
		_, err := pool.Exec(ctx, `
			UPDATE realtime_worker_offers
			   SET available_sequences=max_active_sequences, status='ACTIVE', last_seen_at=now()`)
		return err
	case "service_lease":
		// Terminalise active leases (valid states: COMPLETED/CANCELLED require finalized_at)
		// so prepaidOpenReservationMicros releases, then restore offer capacity.
		if _, err := pool.Exec(ctx, `
			UPDATE service_leases
			   SET state='CANCELLED', finalized_at=COALESCE(finalized_at, now()),
			       expires_at=LEAST(expires_at, now())
			 WHERE state IN ('ACTIVE','UPGRADING','FAILOVER_REQUIRED')`); err != nil {
			return err
		}
		if _, err := pool.Exec(ctx, `
			UPDATE service_lease_worker_offers
			   SET available_warm_replicas=maximum_warm_replicas, last_seen_at=now(), status='READY'`); err != nil {
			return err
		}
		// Top up prepaid for the primary buyer (extra buyers are seeded large).
		return store.SeedPrepaidBalance(ctx, buyerID, 100_000_000_000, "sel-replenish-"+uuid.NewString())
	}
	return nil
}

func ensureQueuedBatchTask(ctx context.Context, pool *pgxpool.Pool, buyerID uuid.UUID) error {
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM tasks WHERE status='queued' AND claimed_by IS NULL`).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	jobID, taskID := uuid.New(), uuid.New()
	if _, err := pool.Exec(ctx, `
		INSERT INTO jobs (id,buyer_id,status,job_type,model_ref,input_ref,task_count,
		                  offered_rate_usd_hr,min_memory_gb,tier,currency)
		VALUES ($1,$2,'running','embed','all-minilm-l6-v2','in',1,10.0,0,'batch','usd')`,
		jobID, buyerID); err != nil {
		return err
	}
	_, err := pool.Exec(ctx, `
		INSERT INTO tasks (id,job_id,status,input_ref,result_key,visible_at)
		VALUES ($1,$2,'queued','in','rk',now())`, taskID, jobID)
	return err
}

func refreshSelectorScaleLiveness(ctx context.Context, pool *pgxpool.Pool, profile VLLMRuntimeProfile, region string) error {
	if _, err := pool.Exec(ctx, `UPDATE workers SET last_seen_at=now()`); err != nil {
		return err
	}
	if _, err := pool.Exec(ctx, `UPDATE realtime_worker_offers SET last_seen_at=now(), status='ACTIVE'`); err != nil {
		// table may be empty / ok
		_ = err
	}
	if region != "" {
		if _, err := pool.Exec(ctx, `
			UPDATE service_lease_worker_offers SET last_seen_at=now(), status='READY'
			 WHERE region=$1`, region); err != nil {
			_ = err
		}
		if _, err := pool.Exec(ctx, `
			UPDATE worker_model_state SET last_seen_warm=now()
			 WHERE model_id=$1`, profile.ModelAlias); err != nil {
			_ = err
		}
	}
	return nil
}

func wipeSelectorScaleTables(ctx context.Context, pool *pgxpool.Pool) error {
	_, err := pool.Exec(ctx, `
		TRUNCATE
		  service_lease_events, service_leases, service_lease_offer_samples, service_lease_worker_offers,
		  realtime_admission_events, realtime_offer_samples, realtime_authorization_events,
		  realtime_settlements, realtime_executions, realtime_refunds, execution_contracts,
		  realtime_worker_offers, realtime_supplier_outcome_stats,
		  task_execution_history, tasks, jobs,
		  worker_authorized_capabilities, worker_model_state, worker_tokens, workers, suppliers
		RESTART IDENTITY CASCADE`)
	// pricing acceptances are append-only (refuse DELETE); TRUNCATE bypasses row
	// triggers. Do it after leases so FKs release.
	if err == nil {
		_, err = pool.Exec(ctx, `TRUNCATE service_lease_pricing_acceptances RESTART IDENTITY CASCADE`)
	}
	return err
}

func tableRowCounts(ctx context.Context, pool *pgxpool.Pool) (map[string]int64, error) {
	tables := []string{
		"suppliers", "workers", "worker_authorized_capabilities", "jobs", "tasks",
		"realtime_worker_offers", "service_lease_worker_offers", "worker_model_state",
		"execution_contracts", "service_leases",
	}
	out := map[string]int64{}
	for _, t := range tables {
		var n int64
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM `+t).Scan(&n); err != nil {
			// missing table on partial schema
			continue
		}
		out[t] = n
	}
	return out, nil
}

func approxDBBytes(ctx context.Context, pool *pgxpool.Pool) (int64, error) {
	var n int64
	err := pool.QueryRow(ctx, `SELECT pg_database_size(current_database())`).Scan(&n)
	return n, err
}

// --- small utilities --------------------------------------------------------

func copyInChunks(ctx context.Context, pool *pgxpool.Pool, total, chunk int, fn func(start, n int) error) error {
	if total < 1 {
		return nil
	}
	if chunk < 1 {
		chunk = total
	}
	for start := 0; start < total; start += chunk {
		n := chunk
		if start+n > total {
			n = total - start
		}
		if err := fn(start, n); err != nil {
			return fmt.Errorf("chunk start=%d n=%d: %w", start, n, err)
		}
	}
	return nil
}

func detUUID(seed int64, lane string, idx int) uuid.UUID {
	sum := sha256.Sum256([]byte(fmt.Sprintf("merc-selector-scale|%d|%s|%d", seed, lane, idx)))
	var u uuid.UUID
	copy(u[:], sum[:16])
	u[6] = (u[6] & 0x0f) | 0x40
	u[8] = (u[8] & 0x3f) | 0x80
	return u
}

func sqlTextDigest(sql string) string {
	sum := sha256.Sum256([]byte(sql))
	return hex.EncodeToString(sum[:])
}

func candleGovernedIdentity() (id, revision, digest string, err error) {
	id, revision, digest, err = governedProfileIdentity("candle")
	if err == nil {
		return id, revision, digest, nil
	}
	profile, ok := runtimeProfileByID("candle_metal")
	if !ok {
		return "", "", "", fmt.Errorf("no candle governed identity: %w", err)
	}
	digest, dErr := profile.CapabilityDigest(runtimeAuthorityModels)
	if dErr != nil {
		return "", "", "", dErr
	}
	return profile.RuntimeID, profile.Revision, digest, nil
}

func latencyFromDurations(samples []time.Duration) selectorScaleLatency {
	if len(samples) == 0 {
		return selectorScaleLatency{}
	}
	ms := make([]float64, len(samples))
	for i, d := range samples {
		ms[i] = float64(d) / float64(time.Millisecond)
	}
	sort.Float64s(ms)
	pct := func(p float64) float64 {
		if len(ms) == 1 {
			return ms[0]
		}
		idx := int(math.Ceil(p*float64(len(ms)))) - 1
		if idx < 0 {
			idx = 0
		}
		if idx >= len(ms) {
			idx = len(ms) - 1
		}
		return ms[idx]
	}
	return selectorScaleLatency{
		P50: pct(0.50), P95: pct(0.95), P99: pct(0.99),
		Min: ms[0], Max: ms[len(ms)-1], N: len(ms),
	}
}

func sumRowCounts(m map[string]int64) int64 {
	var s int64
	for _, v := range m {
		s += v
	}
	return s
}

func selectorMaxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func truncateString(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "\n…[truncated]"
}

func formatSelectorScaleContention(before, after []float64, cpus int) string {
	b := "unknown"
	a := "unknown"
	if len(before) > 0 {
		b = fmt.Sprintf("%.2f", before[0])
	}
	if len(after) > 0 {
		a = fmt.Sprintf("%.2f", after[0])
	}
	note := fmt.Sprintf("loadavg 1m before=%s after=%s on %d CPUs. ", b, a, cpus)
	// Contended if 1-minute load is well above core count.
	if len(before) > 0 && before[0] > float64(cpus)*0.75 {
		note += "Host is contended relative to core count; absolute wall times are PROVISIONAL — report the curve SHAPE, not the milliseconds as SLOs."
	} else if len(after) > 0 && after[0] > float64(cpus)*0.75 {
		note += "Host became contended during the run; absolute wall times are PROVISIONAL — report the curve SHAPE, not the milliseconds as SLOs."
	} else {
		note += "Load was moderate for this host at observation time; still single-host synthetic evidence only, not a fleet SLO."
	}
	return note
}

func mercSourceCommitSHA() string {
	if v := strings.TrimSpace(os.Getenv("MERC_SOURCE_COMMIT")); v != "" {
		return v
	}
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, s := range bi.Settings {
			if s.Key == "vcs.revision" && s.Value != "" {
				return s.Value
			}
		}
	}
	return "unknown"
}

func writeSelectorScaleEvidence(report selectorScaleReport) error {
	// control/ tests run with cwd=control/; evidence lives at repo root.
	//
	// A diagnostic run writes beside the curve, never over it. Overwriting would
	// destroy 1.4 hours of measurement with three cells and leave an artifact
	// whose filename still claims to be the curve.
	rel := selectorScaleEvidenceRel
	if report.Partial != nil {
		rel = selectorScaleDiagnosticEvidenceRel
	}
	path := filepath.Join("..", rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	return os.WriteFile(path, raw, 0o644)
}

// firstErrorText names what a cell refused with, or says plainly that nothing
// was captured — never an empty string that reads as "no error".
func firstErrorText(r selectorScaleSampleResult) string {
	if r.FirstError == nil {
		return "no error text captured"
	}
	return *r.FirstError
}
