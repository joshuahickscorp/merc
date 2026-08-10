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
	selectorScaleSeed            int64 = 20260809
	selectorScaleEnv                   = "MERC_SELECTOR_SCALE_CURVE"
	selectorScaleSamples               = 40
	selectorScaleWarmup                = 5
	selectorScaleConcurrencyHigh       = 8 // multi-poller contention; not host saturation
	selectorScaleJobBacklog            = 200
	selectorScaleCopyChunk             = 20_000
	selectorScaleEvidenceRel           = "evidence/perf/selector-scale-curve.json"
)

var selectorScaleLadder = []int{1_000, 10_000, 100_000, 1_000_000}

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
		TargetsMS: selectorScaleTargetsMS,
		Lanes:     map[string]selectorScaleLane{},
	}

	// Run lanes serially so fleet tables do not fight each other for RAM/disk.
	for _, lane := range []string{"batch", "realtime", "service_lease"} {
		laneReport, refusals := runSelectorScaleLane(t, ctx, store, pool, profile, lane)
		report.Lanes[lane] = laneReport
		report.Refusals = append(report.Refusals, refusals...)
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
	t.Logf("wrote %s (wall=%.1fs load_before=%v load_after=%v)",
		selectorScaleEvidenceRel, report.WallClockSeconds, loadBefore, loadAfter)
}

// --- report types -----------------------------------------------------------

type selectorScaleReport struct {
	SchemaVersion     int                          `json:"schema_version"`
	GeneratedAt       string                       `json:"generated_at"`
	FinishedAt        string                       `json:"finished_at,omitempty"`
	WallClockSeconds  float64                      `json:"wall_clock_seconds,omitempty"`
	SourceCommit      string                       `json:"source_commit"`
	Seed              int64                        `json:"seed"`
	Host              string                       `json:"host"`
	NumCPU            int                          `json:"num_cpu"`
	LoadAverageBefore []float64                    `json:"load_average_before"`
	LoadAverageAfter  []float64                    `json:"load_average_after"`
	Invocation        selectorScaleInvocation      `json:"invocation"`
	Honesty           selectorScaleHonesty         `json:"honesty"`
	TargetsMS         map[int]float64              `json:"bible_targets_ms"`
	Lanes             map[string]selectorScaleLane `json:"lanes"`
	Refusals          []selectorScaleRefusal       `json:"refusals,omitempty"`
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
	EntryPoint          string                  `json:"entry_point"`
	IndependentVariable string                  `json:"independent_variable"`
	SQLTextDigest       string                  `json:"sql_text_digest"`
	SQLStatementName    string                  `json:"sql_statement_name"`
	Points              []selectorScalePoint    `json:"points"`
	PlannerShapeFlips   []selectorScalePlanFlip `json:"planner_shape_flips,omitempty"`
}

type selectorScalePoint struct {
	Scale                    int                             `json:"scale"`
	IndependentVariableValue int                             `json:"independent_variable_value"`
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
	LoadAverageDuring        []float64                       `json:"load_average_during"`
	SeedWallSeconds          float64                         `json:"seed_wall_seconds"`
	MeasureWallSeconds       float64                         `json:"measure_wall_seconds"`
	DBBytesApprox            int64                           `json:"db_bytes_approx,omitempty"`
	Notes                    string                          `json:"notes,omitempty"`
	Status                   string                          `json:"status"` // measured | refused
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
	profile VLLMRuntimeProfile, lane string,
) (selectorScaleLane, []selectorScaleRefusal) {
	t.Helper()
	var out selectorScaleLane
	var refusals []selectorScaleRefusal
	switch lane {
	case "batch":
		out.EntryPoint = "Store.ClaimTasksTx"
		out.IndependentVariable = "fleet_workers_online (EXISTS cheaper_class_online / cheaper_ask_online per eligible job)"
		out.SQLStatementName = "ClaimTaskSQL(claimed_by IS NULL)"
		out.SQLTextDigest = sqlTextDigest(ClaimTaskSQL("t.claimed_by IS NULL"))
	case "realtime":
		out.EntryPoint = "Store.AuthorizeRealtimeContract (offer-claim CTE)"
		out.IndependentVariable = "realtime_worker_offers rows for one runtime_profile_id + runtime_profile_sha256"
		out.SQLStatementName = "realtimeAuthorizeSelectOfferSQLSkip|Blocking"
		out.SQLTextDigest = sqlTextDigest(realtimeAuthorizeSelectOfferSQLSkip + "\n" + realtimeAuthorizeSelectOfferSQLBlocking)
	case "service_lease":
		out.EntryPoint = "Store.CreateServiceLease"
		out.IndependentVariable = "service_lease_worker_offers rows for one runtime_profile_id + region under FOR UPDATE"
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

	for _, scale := range selectorScaleLadder {
		t.Logf("lane=%s scale=%d seeding…", lane, scale)
		point, refuse, err := measureSelectorScalePoint(t, ctx, store, pool, profile, lane, scale, out.SQLTextDigest)
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
) (selectorScalePoint, *selectorScaleRefusal, error) {
	t.Helper()
	point := selectorScalePoint{
		Scale:                    scale,
		IndependentVariableValue: scale,
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
		taskN := selectorMaxInt(jobBacklog, selectorScaleSamples*selectorScaleConcurrencyHigh*2)
		var fleet selectorBatchFleet
		fleet, seedErr = seedSelectorBatchFleet(ctx, pool, scale, selectorScaleSeed, taskN, buyerID)
		claimer = WorkerAuth{WorkerID: fleet.ClaimerWorkerID, SupplierID: fleet.ClaimerSupplierID}
		point.Notes = fmt.Sprintf("fixed job/task backlog=%d; independent variable is fleet workers=%d", taskN, scale)
	case "realtime":
		buyerID, seedErr = store.CreateBuyerAccount(ctx, fmt.Sprintf("sel-r-%d-%s@example.test", scale, uuid.NewString()[:8]),
			"integration-password", 100_000)
		if seedErr != nil {
			return point, nil, seedErr
		}
		seedErr = seedSelectorRealtimeBook(ctx, pool, profile, scale, selectorScaleSeed)
		point.Notes = fmt.Sprintf("offer book size=%d on profile %s", scale, profile.RuntimeProfileID)
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
		point.Notes = fmt.Sprintf("lease offer book size=%d on profile %s region %s", scale, profile.RuntimeProfileID, region)
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
		samples := measureSelectorScaleSamples(t, ctx, store, pool, lane, profile, region, claimer, buyerID, conc, nSamples)
		key := fmt.Sprintf("concurrency_%d", conc)
		point.WallMS[key] = latencyFromDurations(samples)
		if nSamples < selectorScaleSamples {
			point.Notes += fmt.Sprintf("; samples_c%d=%d (reduced at this scale for wall budget; p99 less stable)", conc, nSamples)
		}
		t.Logf("lane=%s scale=%d c=%d p50=%.3fms p95=%.3fms p99=%.3fms n=%d",
			lane, scale, conc, point.WallMS[key].P50, point.WallMS[key].P95, point.WallMS[key].P99, point.WallMS[key].N)
	}
	point.MeasureWallSeconds = time.Since(measureStart).Seconds()
	return point, nil, nil
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

func measureSelectorScaleSamples(
	t *testing.T, ctx context.Context, store *Store, pool *pgxpool.Pool,
	lane string, profile VLLMRuntimeProfile, region string,
	claimer WorkerAuth, buyerID uuid.UUID, concurrency, nSamples int,
) []time.Duration {
	t.Helper()
	if nSamples < 1 {
		nSamples = selectorScaleSamples
	}
	total := selectorScaleWarmup + nSamples
	raw := make([]time.Duration, total)
	var fail atomic.Int32

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

	jobs := make(chan int)
	var wg sync.WaitGroup
	for w := 0; w < concurrency; w++ {
		wg.Add(1)
		go func(workerIdx int) {
			defer wg.Done()
			for idx := range jobs {
				b := buyers[workerIdx%len(buyers)]
				start := time.Now()
				var callErr error
				switch lane {
				case "batch":
					// Ensure a claimable task exists for this sample.
					if err := ensureQueuedBatchTask(ctx, pool, b); err != nil {
						fail.Add(1)
						raw[idx] = time.Since(start)
						continue
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
					_, _, callErr = store.AuthorizeRealtimeContract(ctx, RealtimeContractAuthorization{
						RequestID: "sel-" + uuid.NewString(), BuyerID: b, Profile: profile,
						InputCommitment: strings.Repeat("a", 64), RequestSHA256: strings.Repeat("b", 64),
						MaximumPriceUSD: maxUSD, EstimatedPriceUSD: estUSD, DeadlineAt: time.Now().Add(time.Minute),
						MaximumPromptTokens: maxPrompt, MaximumCompletionTokens: maxCompletion,
						EstimatedPromptTokens: estPrompt, EstimatedCompletionTokens: estCompletion,
						BuyerDeclaredCeilingUSD: maxUSD * 1.1,
					})
				case "service_lease":
					_, callErr = store.CreateServiceLease(ctx, b, ServiceLeaseRequest{
						RuntimeProfileID: profile.RuntimeProfileID, Region: region, Currency: "usd",
						MinimumReplicas: 1, MaximumReplicas: 1, TermSeconds: 60,
						MaximumP95LatencyMilliseconds: 500, BuyerDeclaredCeilingNanos: 135_000_000,
					})
				}
				raw[idx] = time.Since(start)
				if callErr != nil {
					fail.Add(1)
					// Do not t.Error from workers (racy); count only.
				}
			}
		}(w)
	}
	for i := 0; i < total; i++ {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	if n := fail.Load(); n > 0 {
		t.Logf("lane=%s c=%d sample failures=%d/%d (included in wall times)", lane, concurrency, n, total)
	}
	return raw[selectorScaleWarmup:]
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
	need := selectorScaleSamples*selectorScaleConcurrencyHigh*2 - queued
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
	path := filepath.Join("..", selectorScaleEvidenceRel)
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
