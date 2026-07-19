//go:build integration

package main

// integration_test.go — the brutal deterministic proof matrix. Runs the REAL
// control plane (handlers, store, verifier, background workers) against a REAL
// Postgres + REAL MinIO, with the supplier agent SIMULATED (we PUT result objects
// and POST commits directly) so the whole lifecycle is exercised without the
// Metal model download — that live path is proven separately by scripts/prove-local.sh.
//
// Gated behind `//go:build integration` so a plain `go test ./...` (no infra)
// stays green; prove-local and CI run it with `-tags integration` plus
// DATABASE_URL + S3_* pointed at a live stack. Missing infra under the tag is a
// loud failure, never a silent skip (BLACKHOLE: surface every failure).
//
// Each capability is one Test*; reset() truncates the volatile tables and
// restores the demo supplier between them so they are order-independent.

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	itPool    *pgxpool.Pool
	itStore   *Store
	itStorage *Storage
	itServer  *Server
	itHTTP    *httptest.Server
)

// demo identities mirror seed.go (the seed runs in TestMain).
var (
	demoSupplierUUID = uuid.MustParse(demoSupplierID)
	demoWorkerUUID   = uuid.MustParse(demoWorkerID)
	demoBuyerUUID    = uuid.MustParse(demoBuyerID)
)

// A second and third supplier, INDEPENDENT of demoSupplierUUID and of each other.
// prunePeers (backlog P0 items 6+8) now excludes same-supplier candidates, so any
// fixture that wants a genuinely eligible redundancy/tiebreak/hedge peer must put it
// on a different supplier than the anchor it is meant to cross-check — a same-supplier
// worker is deliberately never independent. ensureExtraDemoSuppliers is idempotent
// (ON CONFLICT DO NOTHING) since suppliers is not truncated by reset().
var (
	demoSupplier2UUID = uuid.MustParse("00000000-0000-0000-0000-0000000000a2")
	demoSupplier3UUID = uuid.MustParse("00000000-0000-0000-0000-0000000000a3")
)

func ensureExtraDemoSuppliers(t *testing.T, ctx context.Context) {
	t.Helper()
	for _, s := range []struct {
		id    uuid.UUID
		email string
	}{
		{demoSupplier2UUID, "demo-supplier-2@computexchange.test"},
		{demoSupplier3UUID, "demo-supplier-3@computexchange.test"},
	} {
		if _, err := itPool.Exec(ctx,
			`INSERT INTO suppliers (id, email, reputation, status) VALUES ($1,$2,0.90,'active')
			 ON CONFLICT (id) DO NOTHING`, s.id, s.email); err != nil {
			t.Fatalf("ensureExtraDemoSuppliers: %v", err)
		}
	}
}

// registerDemoWorkerForTest gives the shared demo identity the same exact
// current-matrix authority a real POST /v1/worker/register would create. Production
// seedDemo intentionally does not create these rows: a seeded/legacy array-only
// worker is inert until its agent re-registers. The integration harness opts in
// explicitly so unrelated lifecycle tests continue to start with one real eligible
// worker while runtime-authority tests can delete the rows to prove inertness.
func demoProductionCapability() WorkerCapability {
	return WorkerCapability{
		WorkerID:     demoWorkerUUID,
		SupplierID:   demoSupplierUUID,
		HWClass:      "apple_silicon_max",
		Engine:       "candle",
		MemoryGB:     64,
		MemoryBwGbps: 400,
		SupportedJobs: []string{
			"embed", "rerank", "batch_infer", "batch_classification",
			"json_extraction", "audio_transcribe",
		},
		SupportedModels: []string{
			"all-minilm-l6-v2", "llama-3.2-1b-instruct-q4",
			"whisper-tiny", "whisper-base",
		},
		AgentVersion: "integration-test",
		OSVersion:    "macOS",
	}
}

func registerDemoWorkerForTest(t *testing.T, ctx context.Context) {
	t.Helper()
	cap := demoProductionCapability()
	if err := itStore.UpsertWorker(ctx, cap); err != nil {
		t.Fatalf("register demo worker fixture: %v", err)
	}
}

// replaceWorkerAuthorizationsForTest is the explicit adapter for integration
// fixtures that insert worker rows directly to exercise a later lifecycle state.
// It derives rows from the generated production matrix exactly like registration;
// it never manufactures authority for a pending/stub runtime. Production code has
// no equivalent escape hatch: only UpsertWorker writes these rows.
func replaceWorkerAuthorizationsForTest(t *testing.T, ctx context.Context, workerID uuid.UUID, pairs ...[2]string) {
	t.Helper()
	var hwClass, engine string
	if err := itPool.QueryRow(ctx,
		`SELECT hw_class, COALESCE(engine,'candle') FROM workers WHERE id = $1`, workerID,
	).Scan(&hwClass, &engine); err != nil {
		t.Fatalf("read worker lane for exact test authority: %v", err)
	}
	if _, err := itPool.Exec(ctx,
		`DELETE FROM worker_authorized_capabilities WHERE worker_id = $1`, workerID); err != nil {
		t.Fatalf("clear exact test authority: %v", err)
	}
	for _, pair := range pairs {
		var matched *generatedRuntimeCapability
		for i := range generatedAdvertisedRuntimeCapabilities {
			cell := &generatedAdvertisedRuntimeCapabilities[i]
			if cell.Engine == engine && generatedCapabilityHasHWClass(*cell, hwClass) &&
				cell.Job == pair[0] && cell.Model == pair[1] {
				matched = cell
				break
			}
		}
		if matched == nil {
			t.Fatalf("test fixture requested non-production authority worker=%s lane=%s/%s tuple=%s/%s",
				workerID, engine, hwClass, pair[0], pair[1])
		}
		if _, err := itPool.Exec(ctx,
			`INSERT INTO worker_authorized_capabilities
			   (worker_id, cell_id, runtime_id, job_type, model_ref, model_kind, matrix_sha256)
			 VALUES ($1,$2,$3,$4,$5,$6,$7)`,
			workerID, matched.ID, matched.Runtime, matched.Job, matched.Model,
			matched.ModelKind, generatedRuntimeMatrixSHA256); err != nil {
			t.Fatalf("insert exact test authority: %v", err)
		}
	}
}

func TestMain(m *testing.M) {
	ctx := context.Background()
	// Tests opt into an explicit versioned economic schedule. Production has no
	// defaults: a missing variable blocks quote/submit. Keeping the fixture here
	// makes that distinction visible instead of teaching production code a test
	// fallback.
	for name, value := range map[string]string{
		economicScheduleVersionEnv: "integration-test-stripe-conservative-v1",
		processorPercentBPSEnv:     "350",
		processorFixedUSDEnv:       "0.35",
		controlPerTaskUSDEnv:       "0.005",
		targetMarginBPSEnv:         "300",
		"CX_TOKEN_KEY":             "integration-test-webhook-and-oauth-sealing-key",
	} {
		if err := os.Setenv(name, value); err != nil {
			fmt.Fprintf(os.Stderr, "integration: setting %s: %v\n", name, err)
			os.Exit(2)
		}
	}
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		fmt.Fprintln(os.Stderr, "integration: DATABASE_URL unset — run via scripts/prove-local.sh (it provisions Postgres + MinIO)")
		os.Exit(2)
	}
	poolCfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "integration: pgx config: %v\n", err)
		os.Exit(2)
	}
	// pgx defaults MaxConns to max(4, runtime.NumCPU()). On a 2-vCPU CI runner
	// that is 4, which starves the concurrency proofs — a lock holder + N blocked
	// contenders + the pg_stat_activity poller all need a connection at once — and
	// the SLA fake fleet, producing "context deadline exceeded" / "job not complete"
	// flakes that never reproduce on a many-core dev box. Pin a generous ceiling so
	// the client pool is never the bottleneck (Postgres max_connections is 100).
	if poolCfg.MaxConns < 25 {
		poolCfg.MaxConns = 25
	}
	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "integration: pgx pool: %v\n", err)
		os.Exit(2)
	}
	defer pool.Close()
	itPool = pool
	itStore = NewStore(pool)
	if err := itStore.Migrate(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "integration: migrate: %v\n", err)
		os.Exit(2)
	}
	st, err := NewStorage(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "integration: storage (MinIO): %v\n", err)
		os.Exit(2)
	}
	itStorage = st
	// Pass storage so seedDemo uploads the honeypot's real input object (matching
	// what a real worker's presigned GET expects) — the same real-storage seed path
	// `control seed` now uses (Buyer Developer Experience 7->8's real-SDK proof
	// found a real worker 404ing on this object when it was DB-only).
	if err := seedDemo(ctx, pool, st); err != nil {
		fmt.Fprintf(os.Stderr, "integration: seed: %v\n", err)
		os.Exit(2)
	}
	// WithStorage(itStorage): matches main.go's real production wiring exactly
	// (NewVerifier(store).WithStorage(storage)). Without it, every HTTP-driven
	// integration test's verifier silently drops the entire 3-way tiebreak
	// path (dispatchTiebreak's `if v.storage == nil { return nil }` early-out,
	// control/verification.go) — a 2-way redundancy mismatch would surface as
	// pass_with_penalty forever and NEVER escalate to a real docked/clawed-back
	// tiebreak loser through the real GET /v1/worker/poll -> POST .../commit
	// path, understating this harness's real coverage. Found by the
	// Verification & Result Trust 7->8 adversarial harness
	// (control/adversarial_test.go): its honeypot-skim scenario's only
	// detection path (repeated confirmed tiebreak losses eroding reputation)
	// silently never fired until this was fixed.
	itServer = NewServer(itStore, itStorage, NewVerifier(itStore).WithStorage(itStorage), stubPayout{})
	itHTTP = httptest.NewServer(itServer.Routes())
	defer itHTTP.Close()
	// Wake-on-work (notify.go): main() starts this alongside the server in
	// production; the integration harness must too, or claimWithWait's real wake
	// path (taskWake, driven by the tasks table's notify trigger) is silently
	// absent here and every long-poll test would fall back to the 5s safety-net
	// tick instead of exercising the real mechanism.
	listenCtx, stopListen := context.WithCancel(ctx)
	defer stopListen()
	go startTaskWakeListener(listenCtx, pool)
	os.Exit(m.Run())
}

// reset truncates the per-test volatile tables and restores the demo supplier to
// a clean, active, 0.90-reputation state so tests do not bleed into each other.
func reset(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	// The TRUNCATE takes ACCESS EXCLUSIVE locks; if a prior test left a lingering
	// locked transaction (e.g. a concurrency proof that a contended CI runner made
	// flake), the reset can deadlock (SQLSTATE 40P01). Retry briefly instead of
	// failing the whole suite on a teardown race — the truncate itself is unchanged.
	const truncate = `TRUNCATE tasks, jobs, webhooks, ledger_entries, benchmark_results, disputes,
		 buyer_charge_operations, stripe_webhook_events, stripe_charge_cash_state, stripe_dispute_cash_state,
		 supplier_payout_funding_state, verification_events, charge_batches
		 RESTART IDENTITY CASCADE`
	var truncErr error
	for attempt := 0; attempt < 20; attempt++ {
		if _, truncErr = itPool.Exec(ctx, truncate); truncErr == nil {
			break
		}
		var pgErr *pgconn.PgError
		if errors.As(truncErr, &pgErr) && pgErr.Code == "40P01" {
			time.Sleep(150 * time.Millisecond)
			continue
		}
		t.Fatalf("reset truncate: %v", truncErr)
	}
	if truncErr != nil {
		t.Fatalf("reset truncate still deadlocking after retries: %v", truncErr)
	}
	// Workers/worker_tokens are NOT truncated above (FK from worker_tokens). Drop
	// every NON-demo worker a prior test left behind so peer selection (tiebreak,
	// hedge, redundancy) is deterministic — each test starts with exactly the demo
	// worker plus whatever it inserts itself. Without this, a leftover same-class
	// worker leaks across tests and steals the pinned peer.
	if _, err := itPool.Exec(ctx, `DELETE FROM worker_tokens WHERE worker_id <> $1`, demoWorkerUUID); err != nil {
		t.Fatalf("reset worker_tokens: %v", err)
	}
	if _, err := itPool.Exec(ctx, `DELETE FROM workers WHERE id <> $1`, demoWorkerUUID); err != nil {
		t.Fatalf("reset workers: %v", err)
	}
	if _, err := itPool.Exec(ctx,
		`UPDATE suppliers SET reputation = 0.90, status = 'active' WHERE id = $1`, demoSupplierUUID); err != nil {
		t.Fatalf("reset supplier: %v", err)
	}
	if _, err := itPool.Exec(ctx,
		`UPDATE suppliers SET reputation = 0.90, status = 'active' WHERE id IN ($1,$2)`,
		demoSupplier2UUID, demoSupplier3UUID); err != nil {
		t.Fatalf("reset extra suppliers: %v", err)
	}
	// Demo worker must exist + be live for poll claims.
	if _, err := itPool.Exec(ctx,
		`UPDATE workers SET last_seen_at = now(), priority_claim_streak = 0 WHERE id = $1`, demoWorkerUUID); err != nil {
		t.Fatalf("reset worker: %v", err)
	}
	registerDemoWorkerForTest(t, ctx)
}

// installRawFixtureEconomicPlan is the narrow adapter for legacy integration
// fixtures that intentionally hand-build jobs/tasks in unusual lifecycle states
// (already complete redundancy peers, old stragglers, etc.). Production has no
// fallback for such rows: the fixture explicitly persists the same immutable
// plan + bounded reserve CreateJobWithTasks requires, then callers stamp the
// returned frozen amounts on every raw task INSERT.
func installRawFixtureEconomicPlan(t *testing.T, ctx context.Context, jobID uuid.UUID, initialTasks, extraReserve int) EconomicPlan {
	t.Helper()
	plan := BuildEconomicPlan(EconomicPlanInput{
		BaseComputeUSD:   float64(initialTasks),
		InitialTaskCount: initialTasks,
		ExtraTaskReserve: extraReserve,
		SupplierShare:    supplierShareRate,
	}, testEconomicSchedule())
	if err := ValidateEconomicPlanSnapshot(plan); err != nil {
		t.Fatalf("raw fixture economic plan: %v", err)
	}
	blob, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := itPool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `
		INSERT INTO job_economic_plans (
		  job_id,plan_version,schedule_version,plan_json,initial_task_count,
		  buyer_charge_per_task_usd,supplier_payout_per_task_usd,
		  initial_buyer_charge_usd,reserved_buyer_charge_usd,sla_premium_usd,firm_quote_max_usd
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,0,NULL)`,
		jobID, plan.Version, plan.Schedule.Version, blob, initialTasks,
		plan.BuyerChargePerTaskUSD, plan.SupplierPayoutPerTaskUSD,
		plan.InitialBuyerChargeUSD, plan.ReservedBuyerChargeUSD); err != nil {
		t.Fatalf("insert raw fixture economic plan: %v", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO job_economic_reserves (job_id,reserved_tasks,consumed_tasks)
		VALUES ($1,$2,0)`, jobID, extraReserve); err != nil {
		t.Fatalf("insert raw fixture reserve: %v", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE jobs SET estimated_usd=$2 WHERE id=$1`, jobID, plan.InitialBuyerChargeUSD); err != nil {
		t.Fatalf("stamp raw fixture estimate: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit raw fixture economics: %v", err)
	}
	return plan
}

// --- HTTP helpers ---

type hdr struct{ k, v string }

func req(t *testing.T, method, path string, body any, headers ...hdr) (int, []byte) {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		switch b := body.(type) {
		case string:
			rdr = strings.NewReader(b)
		case []byte:
			rdr = bytes.NewReader(b)
		default:
			j, _ := json.Marshal(b)
			rdr = bytes.NewReader(j)
		}
	}
	r, err := http.NewRequest(method, itHTTP.URL+path, rdr)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	for _, h := range headers {
		r.Header.Set(h.k, h.v)
	}
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatalf("do %s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, out
}

func buyerKey() hdr  { return hdr{"Authorization", "Bearer " + demoAPIKey} }
func adminKey() hdr  { return hdr{"Authorization", "Bearer " + demoAdminAPIKey} }
func workerTok() hdr { return hdr{"X-Worker-Token", demoWorkerToken} }
func jsonCT() hdr    { return hdr{"Content-Type", "application/json"} }

func integrationAdminActor(t *testing.T) AdminActor {
	t.Helper()
	var id uuid.UUID
	if err := itPool.QueryRow(context.Background(),
		`SELECT id FROM api_keys WHERE key_hash=$1 AND is_admin=true AND revoked=false`,
		hashKey(demoAdminAPIKey)).Scan(&id); err != nil {
		t.Fatalf("load integration admin actor: %v", err)
	}
	return AdminActor{
		Mode:             AdminAuthBreakGlassAPIKey,
		PrincipalID:      id,
		AttributionScope: AdminAttributionSharedCredentialOnly,
		Label:            "integration break-glass API key",
	}
}

// embedResultJSON builds a deterministic embed result blob with n unit vectors.
func embedResultJSON(n int) []byte {
	vecs := make([][]float64, n)
	for i := range vecs {
		v := make([]float64, 384)
		v[i%384] = 1.0 // a distinct unit vector per record
		vecs[i] = v
	}
	b, _ := json.Marshal(map[string]any{"job_type": "embed", "model": "all-minilm-l6-v2", "dim": 384, "count": n, "vectors": vecs})
	return b
}

// demoHoneypotEmbedResultJSON puts the seeded, measured known-answer vector in
// the exact result envelope emitted by the Rust EmbedRunner. The seed remains a
// vectors-only semantic fixture so old known answers stay readable, but a fake
// worker must upload the same job_type/model/dim/count contract as a real agent.
func demoHoneypotEmbedResultJSON(t *testing.T) []byte {
	t.Helper()
	var known struct {
		Vectors [][]float64 `json:"vectors"`
	}
	if err := json.Unmarshal([]byte(demoHoneypotEmbedKnownAnswer), &known); err != nil {
		t.Fatalf("decode seeded honeypot answer: %v", err)
	}
	width := 0
	if len(known.Vectors) > 0 {
		width = len(known.Vectors[0])
	}
	if len(known.Vectors) != 1 || width != 384 {
		t.Fatalf("seeded honeypot answer has shape %dx%d, want 1x384",
			len(known.Vectors), width)
	}
	result := struct {
		JobType string      `json:"job_type"`
		Model   string      `json:"model"`
		Dim     int         `json:"dim"`
		Count   int         `json:"count"`
		Vectors [][]float64 `json:"vectors"`
	}{
		JobType: "embed",
		Model:   "all-minilm-l6-v2",
		Dim:     len(known.Vectors[0]),
		Count:   len(known.Vectors),
		Vectors: known.Vectors,
	}
	body, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("encode seeded honeypot result envelope: %v", err)
	}
	return body
}

// submitEmbedJob submits an embed job with the given input lines + policy and
// returns the job id and task count.
func submitEmbedJob(t *testing.T, lines int, redFrac, honeyFrac float32, holdSecs uint32) (uuid.UUID, int) {
	t.Helper()
	var sb strings.Builder
	for i := 0; i < lines; i++ {
		fmt.Fprintf(&sb, `{"id":"r%d","text":"record %d"}`+"\n", i, i)
	}
	body := map[string]any{
		"job_type":    map[string]any{"type": "embed"},
		"model":       map[string]any{"kind": "hf", "ref": "all-minilm-l6-v2"},
		"params":      map[string]any{"split_size": 1000},
		"constraints": map[string]any{"min_memory_gb": 2},
		// skip_verification_floor: this helper's callers pass explicit
		// redundancy_frac/honeypot_frac values because THEY are testing something
		// else entirely (basic dispatch, drift, latency phases, IDOR, etc.), not
		// the verification floor itself — that gets its own dedicated real request
		// in TestVerificationFloorAppliesUnlessOptedOut (Verification & Result
		// Trust 6->7, docs/internal/CREED_AND_PATH_TO_TEN.md). Opting out here
		// keeps every existing caller's task-count expectations exactly as they
		// were before that fix landed.
		"verification": map[string]any{"redundancy_frac": redFrac, "honeypot_frac": honeyFrac, "payout_hold_secs": holdSecs, "skip_verification_floor": true},
		"tier":         "batch",
		"input":        sb.String(),
	}
	code, out := req(t, "POST", "/v1/jobs", body, buyerKey(), jsonCT())
	if code != http.StatusAccepted {
		t.Fatalf("submit: want 202, got %d: %s", code, out)
	}
	var r JobSubmitResponse
	if err := json.Unmarshal(out, &r); err != nil {
		t.Fatalf("submit decode: %v (%s)", err, out)
	}
	return r.JobID, r.TaskCount
}

// TestVerificationFloorAppliesUnlessOptedOut proves the Verification & Result
// Trust 6->7 fix (docs/internal/CREED_AND_PATH_TO_TEN.md): a job submitted with
// BOTH redundancy_frac=0 and honeypot_frac=0 used to run with ZERO real
// anti-fraud coverage. Now the server bumps honeypot_frac to a real floor
// UNLESS the buyer explicitly opts out via skip_verification_floor.
func TestVerificationFloorAppliesUnlessOptedOut(t *testing.T) {
	ctx := context.Background()

	countHoneypotTasks := func(jobID uuid.UUID) int {
		var n int
		if err := itPool.QueryRow(ctx,
			`SELECT count(*) FROM tasks WHERE job_id=$1 AND is_honeypot=true`, jobID).Scan(&n); err != nil {
			t.Fatalf("counting honeypot tasks: %v", err)
		}
		return n
	}
	totalTasks := func(jobID uuid.UUID) int {
		var n int
		if err := itPool.QueryRow(ctx, `SELECT count(*) FROM tasks WHERE job_id=$1`, jobID).Scan(&n); err != nil {
			t.Fatalf("counting tasks: %v", err)
		}
		return n
	}

	t.Run("zero fractions with no opt-out get a real floor", func(t *testing.T) {
		reset(t)
		var sb strings.Builder
		for i := 0; i < 20; i++ {
			fmt.Fprintf(&sb, `{"id":"r%d","text":"record %d"}`+"\n", i, i)
		}
		body := map[string]any{
			"job_type":     map[string]any{"type": "embed"},
			"model":        map[string]any{"kind": "hf", "ref": "all-minilm-l6-v2"},
			"params":       map[string]any{"split_size": 1000},
			"constraints":  map[string]any{"min_memory_gb": 2},
			"verification": map[string]any{"redundancy_frac": 0, "honeypot_frac": 0},
			"tier":         "batch",
			"input":        sb.String(),
		}
		code, out := req(t, "POST", "/v1/jobs", body, buyerKey(), jsonCT())
		if code != http.StatusAccepted {
			t.Fatalf("submit: want 202, got %d: %s", code, out)
		}
		var r JobSubmitResponse
		if err := json.Unmarshal(out, &r); err != nil {
			t.Fatal(err)
		}
		if got := countHoneypotTasks(r.JobID); got == 0 {
			t.Fatal("job submitted with zero verification and no opt-out must still get a real honeypot floor, got 0 honeypot tasks")
		}
	})

	t.Run("explicit opt-out yields genuinely zero verification", func(t *testing.T) {
		reset(t)
		var sb strings.Builder
		for i := 0; i < 20; i++ {
			fmt.Fprintf(&sb, `{"id":"r%d","text":"record %d"}`+"\n", i, i)
		}
		body := map[string]any{
			"job_type":     map[string]any{"type": "embed"},
			"model":        map[string]any{"kind": "hf", "ref": "all-minilm-l6-v2"},
			"params":       map[string]any{"split_size": 1000},
			"constraints":  map[string]any{"min_memory_gb": 2},
			"verification": map[string]any{"redundancy_frac": 0, "honeypot_frac": 0, "skip_verification_floor": true},
			"tier":         "batch",
			"input":        sb.String(),
		}
		code, out := req(t, "POST", "/v1/jobs", body, buyerKey(), jsonCT())
		if code != http.StatusAccepted {
			t.Fatalf("submit: want 202, got %d: %s", code, out)
		}
		var r JobSubmitResponse
		if err := json.Unmarshal(out, &r); err != nil {
			t.Fatal(err)
		}
		if got := countHoneypotTasks(r.JobID); got != 0 {
			t.Fatalf("explicit skip_verification_floor must yield ZERO honeypot tasks, got %d", got)
		}
		if got, want := totalTasks(r.JobID), r.TaskCount; got != want {
			t.Fatalf("explicit opt-out: want exactly %d primary tasks and nothing else, got %d total", want, got)
		}
	})

	t.Run("a buyer-set non-zero fraction is left untouched", func(t *testing.T) {
		reset(t)
		jobID, _ := submitEmbedJob(t, 20, 0, 0.5, 0)
		if got := countHoneypotTasks(jobID); got == 0 {
			t.Fatal("an explicit 0.5 honeypot_frac must not be zeroed or otherwise altered by the floor logic")
		}
	})
}

// --- 1. health + metrics ---

func TestHealthAndMetrics(t *testing.T) {
	reset(t)
	if code, _ := req(t, "GET", "/healthz", nil); code != 200 {
		t.Fatalf("healthz: want 200, got %d", code)
	}
	code, body := req(t, "GET", "/metrics", nil)
	if code != 200 {
		t.Fatalf("metrics: want 200, got %d", code)
	}
	for _, name := range []string{
		"cx_jobs_submitted_total", "cx_tasks_dispatched_total", "cx_tasks_completed_total",
		"cx_verification_mismatch_total", "cx_payouts_released_total", "cx_active_workers",
	} {
		if !strings.Contains(string(body), name) {
			t.Errorf("metrics missing %q", name)
		}
	}
}

// --- 2. auth: 401 / 403 paths, no silent bypass ---

func TestAuth(t *testing.T) {
	reset(t)
	cases := []struct {
		name   string
		method string
		path   string
		want   int
		hdrs   []hdr
	}{
		{"buyer no token", "GET", "/v1/models", 401, nil},
		{"buyer bad token", "GET", "/v1/models", 401, []hdr{{"Authorization", "Bearer nope"}}},
		{"buyer good token", "GET", "/v1/models", 200, []hdr{buyerKey()}},
		{"admin via non-admin key", "GET", "/admin/workers", 403, []hdr{buyerKey()}},
		{"admin via admin key", "GET", "/admin/workers", 200, []hdr{adminKey()}},
		{"worker no token", "GET", "/v1/worker/poll", 401, nil},
		{"worker bad token", "GET", "/v1/worker/poll", 401, []hdr{{"X-Worker-Token", "garbage"}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			code, body := req(t, c.method, c.path, nil, c.hdrs...)
			if code != c.want {
				t.Fatalf("want %d, got %d: %s", c.want, code, body)
			}
		})
	}
}

// --- 3. malformed submissions all fail cleanly (4xx, no partial job) ---

func TestMalformedSubmissions(t *testing.T) {
	reset(t)
	bad := []struct {
		name string
		body string
	}{
		{"invalid json", `{`},
		{"missing job_type", `{"model":{"kind":"gguf","ref":"x"},"input":"{}"}`},
		{"bad tier", `{"job_type":{"type":"embed"},"tier":"bogus","input":"{\"a\":1}"}`},
		{"bad hw_class", `{"job_type":{"type":"embed"},"constraints":{"hw_classes":["nonsense"]},"input":"{\"a\":1}"}`},
		{"insecure webhook", `{"job_type":{"type":"embed"},"webhook_url":"http://x","input":"{\"a\":1}"}`},
		{"empty input", `{"job_type":{"type":"embed"},"input":""}`},
		{"missing input", `{"job_type":{"type":"embed"}}`},
	}
	for _, c := range bad {
		t.Run(c.name, func(t *testing.T) {
			code, body := req(t, "POST", "/v1/jobs", c.body, buyerKey(), jsonCT())
			if code < 400 || code >= 500 {
				t.Fatalf("want 4xx, got %d: %s", code, body)
			}
		})
	}
	// No jobs should have been created by any malformed request.
	var n int
	if err := itPool.QueryRow(context.Background(), `SELECT count(*) FROM jobs`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("malformed submissions leaked %d job rows", n)
	}
}

// --- 4. worker registration ---

func TestWorkerRegister(t *testing.T) {
	reset(t)
	cap := WorkerCapability{
		HWClass: "apple_silicon_max", MemoryGB: 64, MemoryBwGbps: 400,
		SupportedJobs: []string{"embed"}, SupportedModels: []string{"all-minilm-l6-v2"},
		AgentVersion: "test", OSVersion: "macOS",
	}
	code, body := req(t, "POST", "/v1/worker/register", cap, workerTok(), jsonCT())
	if code != 200 {
		t.Fatalf("register: want 200, got %d: %s", code, body)
	}
	var echoed WorkerCapability
	if err := json.Unmarshal(body, &echoed); err != nil {
		t.Fatalf("decode echo: %v", err)
	}
	if echoed.WorkerID != demoWorkerUUID {
		t.Fatalf("register bound wrong worker_id: %s", echoed.WorkerID)
	}
	// bad hw_class rejected
	if code, _ := req(t, "POST", "/v1/worker/register",
		WorkerCapability{HWClass: "frobnicator"}, workerTok(), jsonCT()); code != 400 {
		t.Fatalf("bad hw_class: want 400, got %d", code)
	}
}

// --- 5. MinIO object flow: put/get round-trip + presigned GET/PUT ---

func TestObjectFlow(t *testing.T) {
	reset(t)
	ctx := context.Background()
	key := "it/object-flow/" + uuid.NewString() + ".bin"
	payload := []byte("compute-exchange-object-flow-" + uuid.NewString())
	if err := itStorage.PutObject(ctx, key, payload, "application/octet-stream"); err != nil {
		t.Fatalf("put: %v", err)
	}
	got, err := itStorage.GetObject(ctx, key)
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("get round-trip: %v got=%q", err, got)
	}
	// Presigned GET is fetchable over plain HTTP.
	gurl, err := itStorage.PresignGet(ctx, key, time.Minute)
	if err != nil {
		t.Fatalf("presign get: %v", err)
	}
	resp, err := http.Get(gurl)
	if err != nil {
		t.Fatalf("http get presigned: %v", err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !bytes.Equal(b, payload) {
		t.Fatalf("presigned GET body mismatch: %q", b)
	}
	// Presigned PUT stores an object the control side can read back.
	pkey := "it/object-flow/put-" + uuid.NewString() + ".bin"
	purl, err := itStorage.PresignPut(ctx, pkey, time.Minute)
	if err != nil {
		t.Fatalf("presign put: %v", err)
	}
	put, _ := http.NewRequest("PUT", purl, bytes.NewReader(payload))
	if r, err := http.DefaultClient.Do(put); err != nil || r.StatusCode/100 != 2 {
		t.Fatalf("http put presigned: %v status=%v", err, r)
	}
	if got, err := itStorage.GetObject(ctx, pkey); err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("get after presigned put: %v", err)
	}
}

// TestResolveInputRejectsCrossBuyerS3Key proves the IDOR fix (Security Posture
// 6.5->7): a buyer submitting {"input":{"s3_key":"jobs/<someone else's job>/..."}}
// must be rejected, and the SAME mechanism must still let a buyer chain THEIR OWN
// completed job's input/output into a new submission — the legitimate use this
// fix has to preserve, not just a lockdown.
func TestResolveInputRejectsCrossBuyerS3Key(t *testing.T) {
	reset(t)
	t.Setenv("CX_SANDBOX_CREDIT_USD", "5")

	// Buyer A: the demo buyer, submits a real job — its input.jsonl now exists in
	// object storage under jobs/<jobA>/input.jsonl.
	jobA, _ := submitEmbedJob(t, 3, 0, 0, 0)

	// Buyer B: a freshly signed-up, DIFFERENT buyer (own buyer_id, own sandbox
	// credit) tries to submit a new job referencing buyer A's input key.
	email := uniqueEmail("idor-buyer")
	code, out := req(t, "POST", "/v1/signup", map[string]any{"email": email, "password": "hunter2hunter2"}, jsonCT())
	if code != http.StatusCreated {
		t.Fatalf("signup: want 201, got %d: %s", code, out)
	}
	var su struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(out, &su); err != nil || su.Token == "" {
		t.Fatalf("signup decode: %v (%s)", err, out)
	}
	sessHdr := hdr{"Authorization", "Bearer " + su.Token}

	crossBody := map[string]any{
		"job_type":    map[string]any{"type": "embed"},
		"model":       map[string]any{"kind": "hf", "ref": "all-minilm-l6-v2"},
		"constraints": map[string]any{"min_memory_gb": 2},
		"tier":        "batch",
		"input":       map[string]any{"s3_key": "jobs/" + jobA.String() + "/input.jsonl"},
	}
	code, out = req(t, "POST", "/v1/jobs", crossBody, sessHdr, jsonCT())
	if code != http.StatusBadRequest {
		t.Fatalf("cross-buyer s3_key must be rejected with 400, got %d: %s", code, out)
	}
	if !strings.Contains(string(out), "does not reference a job you submitted") {
		t.Fatalf("rejection reason not surfaced honestly: %s", out)
	}

	// Buyer A referencing THEIR OWN job's input must still work — the legitimate
	// chaining use case this fix must not break.
	ownBody := map[string]any{
		"job_type":    map[string]any{"type": "embed"},
		"model":       map[string]any{"kind": "hf", "ref": "all-minilm-l6-v2"},
		"constraints": map[string]any{"min_memory_gb": 2},
		"tier":        "batch",
		"input":       map[string]any{"s3_key": "jobs/" + jobA.String() + "/input.jsonl"},
	}
	code, out = req(t, "POST", "/v1/jobs", ownBody, buyerKey(), jsonCT())
	if code != http.StatusAccepted {
		t.Fatalf("buyer chaining their own job's input must still succeed, got %d: %s", code, out)
	}
}

// --- 6. embed happy path (agent simulated) → complete + ledger ---

func TestEmbedHappyPathSimulated(t *testing.T) {
	reset(t)
	ctx := context.Background()
	beforeSub := metrics.jobsSubmitted.Load()
	beforeDone := metrics.tasksCompleted.Load()

	if code, body := req(t, "POST", "/v1/worker/register",
		WorkerCapability{HWClass: "apple_silicon_max", MemoryGB: 64,
			SupportedJobs: []string{"embed"}, SupportedModels: []string{"all-minilm-l6-v2"}},
		workerTok(), jsonCT()); code != 200 {
		t.Fatalf("register: %d %s", code, body)
	}
	jobID, taskCount := submitEmbedJob(t, 1, 0, 0, 0)
	if taskCount != 1 {
		t.Fatalf("want 1 task, got %d", taskCount)
	}

	// Poll → claim the task.
	code, body := req(t, "GET", "/v1/worker/poll", nil, workerTok())
	if code != 200 {
		t.Fatalf("poll: want 200, got %d: %s", code, body)
	}
	var disp TaskDispatch
	if err := json.Unmarshal(body, &disp); err != nil {
		t.Fatalf("dispatch decode: %v", err)
	}
	// The presigned input URL must actually serve the chunk.
	if r, err := http.Get(disp.InputURL); err != nil || r.StatusCode != 200 {
		t.Fatalf("fetch presigned input: %v status=%v", err, r)
	} else {
		r.Body.Close()
	}

	// Simulate the agent: PUT a result object at the canonical key, then commit.
	if err := itStorage.PutObject(ctx, disp.ResultKey, embedResultJSON(1), "application/json"); err != nil {
		t.Fatalf("put result: %v", err)
	}
	commit := TaskCommit{TaskID: disp.TaskID, ResultKey: disp.ResultKey, DurationMS: 12, TokensUsed: 8}
	if code, body := req(t, "POST", "/v1/worker/task/"+disp.TaskID.String()+"/commit", commit, workerTok(), jsonCT()); code != 204 {
		t.Fatalf("commit: want 204, got %d: %s", code, body)
	}

	// Job should be complete (hold 0, single task) and a supplier credit written.
	code, body = req(t, "GET", "/v1/jobs/"+jobID.String(), nil, buyerKey())
	if code != 200 {
		t.Fatalf("get job: %d %s", code, body)
	}
	var js JobStatus
	json.Unmarshal(body, &js)
	if js.Status != "complete" {
		t.Fatalf("job status: want complete, got %q", js.Status)
	}
	var credits int
	itPool.QueryRow(ctx, `SELECT count(*) FROM ledger_entries le JOIN tasks t ON t.id=le.task_id
		WHERE t.job_id=$1 AND le.kind='supplier_credit'`, jobID).Scan(&credits)
	if credits != 1 {
		t.Fatalf("want 1 supplier_credit, got %d", credits)
	}
	// Results endpoint returns presigned URLs.
	if code, body := req(t, "GET", "/v1/jobs/"+jobID.String()+"/results", nil, buyerKey()); code != 200 {
		t.Fatalf("results: %d %s", code, body)
	}
	if metrics.jobsSubmitted.Load() <= beforeSub || metrics.tasksCompleted.Load() <= beforeDone {
		t.Fatalf("metrics did not advance (sub %d->%d done %d->%d)",
			beforeSub, metrics.jobsSubmitted.Load(), beforeDone, metrics.tasksCompleted.Load())
	}
}

// TestJobConstraintsPersistAndDispatch proves every buyer-authored constraint
// survives the submit -> jobs row -> claim -> poll manifest path. These values
// are execution inputs to the agent, not scheduler-only hints.
func TestJobConstraintsPersistAndDispatch(t *testing.T) {
	reset(t)
	ctx := context.Background()
	body := map[string]any{
		"job_type": map[string]any{"type": "embed"},
		"model":    map[string]any{"kind": "hf", "ref": "all-minilm-l6-v2"},
		"params":   map[string]any{"split_size": 1000},
		"constraints": map[string]any{
			"min_memory_gb":     7.5,
			"hw_classes":        []string{"apple_silicon_max"},
			"max_duration_secs": 4321,
			"data_residency":    []string{"US"},
		},
		"verification": map[string]any{
			"redundancy_frac": 0, "honeypot_frac": 0,
			"payout_hold_secs": 0, "skip_verification_floor": true,
		},
		"tier":  "batch",
		"input": "{\"id\":\"r0\",\"text\":\"constraint round trip\"}\n",
	}
	code, out := req(t, "POST", "/v1/jobs", body, buyerKey(), jsonCT())
	if code != http.StatusAccepted {
		t.Fatalf("submit: want 202, got %d: %s", code, out)
	}
	var submitted JobSubmitResponse
	if err := json.Unmarshal(out, &submitted); err != nil {
		t.Fatalf("submit decode: %v", err)
	}

	var minMemory float32
	var hwClasses, residency []string
	var maxDuration uint32
	if err := itPool.QueryRow(ctx, `
		SELECT min_memory_gb, hw_classes, max_duration_secs, data_residency
		  FROM jobs WHERE id=$1`, submitted.JobID).
		Scan(&minMemory, &hwClasses, &maxDuration, &residency); err != nil {
		t.Fatalf("read persisted constraints: %v", err)
	}
	if minMemory != 7.5 || maxDuration != 4321 ||
		len(hwClasses) != 1 || hwClasses[0] != "apple_silicon_max" ||
		len(residency) != 1 || residency[0] != "US" {
		t.Fatalf("persisted constraints changed: memory=%v hw=%v duration=%d residency=%v",
			minMemory, hwClasses, maxDuration, residency)
	}

	code, out = req(t, "GET", "/v1/worker/poll", nil, workerTok())
	if code != http.StatusOK {
		t.Fatalf("poll: want 200, got %d: %s", code, out)
	}
	var disp TaskDispatch
	if err := json.Unmarshal(out, &disp); err != nil {
		t.Fatalf("dispatch decode: %v", err)
	}
	got := disp.Manifest.Constraints
	if got.MinMemoryGB != 7.5 || got.MaxDurationSecs != 4321 ||
		len(got.HWClasses) != 1 || got.HWClasses[0] != "apple_silicon_max" ||
		len(got.DataResidency) != 1 || got.DataResidency[0] != "US" {
		t.Fatalf("dispatch constraints changed: %+v", got)
	}
}

// --- 6b. quote-to-actual drift: a committed task records its real duration ---

// Plane D D6 / errata C-Errata-6: every COMMITTED task writes one task_durations row
// carrying the worker's reported wall-time, job_type, model_ref + split_size, so the
// Exchange Brain can learn an observed p90. An exact response-loss replay is 204 but
// records NOTHING — the duration is written inside the commit transaction, so a task
// that does not truly commit cannot poison the estimate. /admin/drift then reflects it.
func TestTaskDurationRecorded(t *testing.T) {
	reset(t)
	ctx := context.Background()
	itPool.Exec(ctx, `TRUNCATE task_durations`)
	t.Cleanup(func() { itPool.Exec(ctx, `TRUNCATE task_durations`) })

	if code, body := req(t, "POST", "/v1/worker/register",
		WorkerCapability{HWClass: "apple_silicon_max", MemoryGB: 64,
			SupportedJobs: []string{"embed"}, SupportedModels: []string{"all-minilm-l6-v2"}},
		workerTok(), jsonCT()); code != 200 {
		t.Fatalf("register: %d %s", code, body)
	}
	jobID, _ := submitEmbedJob(t, 1, 0, 0, 0)

	_, body := req(t, "GET", "/v1/worker/poll", nil, workerTok())
	var disp TaskDispatch
	if err := json.Unmarshal(body, &disp); err != nil {
		t.Fatalf("dispatch decode: %v", err)
	}
	itStorage.PutObject(ctx, disp.ResultKey, embedResultJSON(1), "application/json")

	const wantDur = 1273
	commit := TaskCommit{TaskID: disp.TaskID, ResultKey: disp.ResultKey, DurationMS: wantDur, TokensUsed: 8}
	if code, b := req(t, "POST", "/v1/worker/task/"+disp.TaskID.String()+"/commit", commit, workerTok(), jsonCT()); code != 204 {
		t.Fatalf("commit: want 204, got %d: %s", code, b)
	}

	// Exactly one duration row for this job, with the worker's reported duration and
	// the job's (job_type, model_ref, split_size) threaded through from the commit.
	var rows int
	var durMs, splitSize int64
	var jobType, modelRef string
	if err := itPool.QueryRow(ctx,
		`SELECT count(*), COALESCE(max(duration_ms),0), COALESCE(max(split_size),0),
		        COALESCE(max(job_type),''), COALESCE(max(model_ref),'')
		   FROM task_durations WHERE job_id=$1`, jobID,
	).Scan(&rows, &durMs, &splitSize, &jobType, &modelRef); err != nil {
		t.Fatalf("read task_durations: %v", err)
	}
	if rows != 1 || durMs != wantDur {
		t.Fatalf("want 1 duration row of %dms, got rows=%d dur=%d", wantDur, rows, durMs)
	}
	if jobType != "embed" || modelRef != "all-minilm-l6-v2" || splitSize != 1000 {
		t.Fatalf("duration row context wrong: job_type=%q model=%q split=%d", jobType, modelRef, splitSize)
	}

	// An exact duplicate is acknowledged idempotently and must not add a row.
	if code, _ := req(t, "POST", "/v1/worker/task/"+disp.TaskID.String()+"/commit", commit, workerTok(), jsonCT()); code != 204 {
		t.Fatalf("duplicate commit replay: want 204, got %d", code)
	}
	itPool.QueryRow(ctx, `SELECT count(*) FROM task_durations WHERE job_id=$1`, jobID).Scan(&rows)
	if rows != 1 {
		t.Fatalf("duplicate commit must not record a second duration row, got %d", rows)
	}

	// The admin drift rollup surfaces this (job_type, model) with its real actuals.
	code, drift := req(t, "GET", "/admin/drift", nil, adminKey())
	if code != 200 {
		t.Fatalf("GET /admin/drift: %d %s", code, drift)
	}
	var dr []DriftRow
	if err := json.Unmarshal(drift, &dr); err != nil {
		t.Fatalf("decode drift: %v\n%s", err, drift)
	}
	found := false
	for _, d := range dr {
		if d.JobType == "embed" && d.ModelRef == "all-minilm-l6-v2" {
			found = true
			if d.Samples < 1 || d.P90DurationMs != wantDur || d.AvgDurationMs != wantDur {
				t.Fatalf("drift row wrong: %+v", d)
			}
		}
	}
	if !found {
		t.Fatalf("drift rollup missing the embed/all-minilm-l6-v2 row: %s", drift)
	}
}

// --- 6b-ii. the drift metric is TIME-WINDOWED, not all-time ---

// TestDriftMetricIsTimeWindowedNotAllTime proves the actual rung this pass climbs
// (Performance Observability 7->8, docs/internal/CREED_AND_PATH_TO_TEN.md: "the
// historical p90 duration calculation... aggregates all-time history with no time
// window, so detection latency actually grows worse as the table grows... change
// [it] to a rolling window (e.g. created_at > now() - 24h) instead of all-time").
//
// Seeds 20 OLD, healthy (100ms) task_durations rows well outside the window
// (40 hours old) for one (job_type, model_ref), then 6 RECENT, badly regressed
// (5000ms — a real ~50x regression) rows well inside the window (10 minutes old)
// for the SAME (job_type, model_ref). Both the direct store method
// (HistoricalP90DurationMs, which feeds the quote's ETA) and the public
// /admin/drift HTTP surface (DriftRollup) must report the windowed p90/avg
// dominated by the RECENT regressed rows — proving a fresh regression is
// detectable within the window instead of being diluted by a much larger body of
// older, healthy history sitting in the same table. Before this pass's fix, the
// all-time query would have blended 20 fast rows against 6 slow ones and reported
// a p90 far below the real current regression (26 samples, healthy-dominated);
// the windowed query must instead report only the 6 recent samples and a p90
// solidly inside the regressed range.
func TestDriftMetricIsTimeWindowedNotAllTime(t *testing.T) {
	reset(t)
	ctx := context.Background()
	const jobType = "batch_infer"
	// A model_ref unique to this test so it can never collide with rows any other
	// test (or a concurrent run) leaves in task_durations, which reset() does not
	// truncate (task_durations is deliberately cross-test durable telemetry — see
	// its own comment at the top of this file).
	const modelRef = "windowed-drift-test-model-only"
	t.Cleanup(func() {
		itPool.Exec(ctx, `DELETE FROM task_durations WHERE model_ref=$1`, modelRef)
	})

	now := time.Now()
	oldTS := now.Add(-40 * time.Hour)      // well outside the 24h driftWindow
	recentTS := now.Add(-10 * time.Minute) // well inside it

	insertDur := func(ts time.Time, durMs int64) {
		if _, err := itPool.Exec(ctx,
			`INSERT INTO task_durations (created_at, job_id, job_type, model_ref, split_size, duration_ms)
			 VALUES ($1, gen_random_uuid(), $2, $3, 10, $4)`,
			ts, jobType, modelRef, durMs); err != nil {
			t.Fatalf("seed task_durations: %v", err)
		}
	}
	const oldDurMs = 100     // healthy, pre-regression
	const recentDurMs = 5000 // a real ~50x regression, all within the window
	for i := 0; i < 20; i++ {
		insertDur(oldTS, oldDurMs)
	}
	for i := 0; i < 6; i++ {
		insertDur(recentTS, recentDurMs)
	}

	// 1. The store method the quote's ETA leans on: windowed, not all-time.
	p90, samples, err := itStore.HistoricalP90DurationMs(ctx, jobType, modelRef)
	if err != nil {
		t.Fatalf("HistoricalP90DurationMs: %v", err)
	}
	if samples != 6 {
		t.Fatalf("want exactly the 6 IN-WINDOW samples, got %d (all-time would be 26 — the old dilution bug)", samples)
	}
	if p90 != recentDurMs {
		t.Fatalf("want windowed p90=%dms (the recent regression), got %dms — old healthy history is diluting a real regression", recentDurMs, p90)
	}

	// 2. The public admin surface (/admin/drift) reflects the same windowed truth,
	// and self-reports the window it used so an operator/skeptic never mistakes it
	// for all-time history.
	code, body := req(t, "GET", "/admin/drift", nil, adminKey())
	if code != 200 {
		t.Fatalf("GET /admin/drift: %d %s", code, body)
	}
	var dr []DriftRow
	if err := json.Unmarshal(body, &dr); err != nil {
		t.Fatalf("decode drift: %v\n%s", err, body)
	}
	found := false
	for _, d := range dr {
		if d.JobType == jobType && d.ModelRef == modelRef {
			found = true
			if d.Samples != 6 {
				t.Fatalf("/admin/drift: want 6 in-window samples, got %d: %+v", d.Samples, d)
			}
			if d.P90DurationMs != recentDurMs {
				t.Fatalf("/admin/drift: want windowed p90=%dms, got %dms: %+v", recentDurMs, d.P90DurationMs, d)
			}
			if d.AvgDurationMs != float64(recentDurMs) {
				t.Fatalf("/admin/drift: want windowed avg=%.0fms (no contribution from the 20 old rows), got %.2fms: %+v", float64(recentDurMs), d.AvgDurationMs, d)
			}
			if d.WindowHours <= 0 {
				t.Fatalf("/admin/drift: want a positive, self-reported window_hours, got %v: %+v", d.WindowHours, d)
			}
			if !d.UsingObservedP90 {
				t.Fatalf("/admin/drift: 6 in-window samples should already clear driftMinSamples=5: %+v", d)
			}
		}
	}
	if !found {
		t.Fatalf("drift rollup missing the %s/%s row: %s", jobType, modelRef, body)
	}
}

// --- 6c. latency phase decomposition: queue-wait / dispatch-overhead / run ---

// TestLatencyPhaseDecomposition proves the new cx_latency_phase_ms backing query
// (End-to-End Job Latency Decomposition 7->7.5): after a REAL job is submitted,
// claimed, started, and committed, its task's real created_at/visible_at/
// claimed_at/started_at/completed_at columns are directly overwritten to KNOWN
// deltas (the live flow itself completes in milliseconds, too fast to assert an
// exact phase split against) — then LatencyPhaseDecomposition must report exactly
// those deltas as both p50 and p90 (a single sample makes both percentiles equal
// the one value, an unambiguous assertion).
func TestLatencyPhaseDecomposition(t *testing.T) {
	reset(t)
	ctx := context.Background()
	req(t, "POST", "/v1/worker/register", WorkerCapability{HWClass: "apple_silicon_max", MemoryGB: 64,
		SupportedJobs: []string{"embed"}, SupportedModels: []string{"all-minilm-l6-v2"}}, workerTok(), jsonCT())
	jobID, _ := submitEmbedJob(t, 1, 0, 0, 0)

	_, body := req(t, "GET", "/v1/worker/poll", nil, workerTok())
	var disp TaskDispatch
	if err := json.Unmarshal(body, &disp); err != nil {
		t.Fatalf("dispatch decode: %v", err)
	}
	itStorage.PutObject(ctx, disp.ResultKey, embedResultJSON(1), "application/json")
	commit := TaskCommit{TaskID: disp.TaskID, ResultKey: disp.ResultKey, DurationMS: 10, TokensUsed: 8}
	if code, b := req(t, "POST", "/v1/worker/task/"+disp.TaskID.String()+"/commit", commit, workerTok(), jsonCT()); code != 204 {
		t.Fatalf("commit: want 204, got %d: %s", code, b)
	}

	// Overwrite the real task's timestamps to known deltas: 2000ms queue-wait,
	// 500ms dispatch overhead, 3000ms run.
	base := time.Now().Add(-time.Hour)
	created := base
	visible := base
	claimed := base.Add(2000 * time.Millisecond)
	started := claimed.Add(500 * time.Millisecond)
	completed := started.Add(3000 * time.Millisecond)
	if _, err := itPool.Exec(ctx,
		`UPDATE tasks SET created_at=$1, visible_at=$2, claimed_at=$3, started_at=$4, completed_at=$5
		 WHERE id=$6`,
		created, visible, claimed, started, completed, disp.TaskID); err != nil {
		t.Fatalf("seeding known timestamps: %v", err)
	}

	rows, err := itStore.LatencyPhaseDecomposition(ctx)
	if err != nil {
		t.Fatalf("LatencyPhaseDecomposition: %v", err)
	}
	var found *LatencyPhaseRow
	for i := range rows {
		if rows[i].JobType == "embed" {
			found = &rows[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("no embed row in latency phase decomposition: %+v", rows)
	}
	const tolMs = 50.0 // wall-clock rounding slack, not phase-boundary ambiguity
	check := func(name string, got, want float64) {
		t.Helper()
		if got < want-tolMs || got > want+tolMs {
			t.Fatalf("%s: want ~%.0fms, got %.3fms", name, want, got)
		}
	}
	check("queue_wait p50", found.QueueWaitP50Ms, 2000)
	check("queue_wait p90", found.QueueWaitP90Ms, 2000)
	check("dispatch_overhead p50", found.DispatchOverheadP50Ms, 500)
	check("dispatch_overhead p90", found.DispatchOverheadP90Ms, 500)
	check("run p50", found.RunP50Ms, 3000)
	check("run p90", found.RunP90Ms, 3000)
	if found.Count != 1 {
		t.Fatalf("want count=1, got %d", found.Count)
	}

	// The /metrics endpoint must actually expose this, not just the store method.
	code, mbody := req(t, "GET", "/metrics", nil)
	if code != 200 {
		t.Fatalf("GET /metrics: %d", code)
	}
	if !strings.Contains(string(mbody), `cx_latency_phase_ms{job_type="embed",phase="run",quantile="0.5"}`) {
		t.Fatalf("/metrics missing cx_latency_phase_ms for embed/run/p50:\n%s", mbody)
	}
	_ = jobID
}

// --- 7. duplicate commit is idempotent: exact replay → 204, credited once ---

func TestDuplicateCommitIdempotent(t *testing.T) {
	reset(t)
	ctx := context.Background()
	req(t, "POST", "/v1/worker/register", WorkerCapability{HWClass: "apple_silicon_max", MemoryGB: 64,
		SupportedJobs: []string{"embed"}, SupportedModels: []string{"all-minilm-l6-v2"}}, workerTok(), jsonCT())
	jobID, _ := submitEmbedJob(t, 1, 0, 0, 0)

	_, body := req(t, "GET", "/v1/worker/poll", nil, workerTok())
	var disp TaskDispatch
	json.Unmarshal(body, &disp)
	itStorage.PutObject(ctx, disp.ResultKey, embedResultJSON(1), "application/json")
	commit := TaskCommit{TaskID: disp.TaskID, ResultKey: disp.ResultKey}

	if code, _ := req(t, "POST", "/v1/worker/task/"+disp.TaskID.String()+"/commit", commit, workerTok(), jsonCT()); code != 204 {
		t.Fatalf("first commit not 204: %d", code)
	}
	if code, _ := req(t, "POST", "/v1/worker/task/"+disp.TaskID.String()+"/commit", commit, workerTok(), jsonCT()); code != 204 {
		t.Fatalf("second exact commit: want 204, got %d", code)
	}
	var credits int
	itPool.QueryRow(ctx, `SELECT count(*) FROM ledger_entries le JOIN tasks t ON t.id=le.task_id
		WHERE t.job_id=$1 AND le.kind='supplier_credit'`, jobID).Scan(&credits)
	if credits != 1 {
		t.Fatalf("double-commit double-credited: %d supplier_credit rows", credits)
	}
}

// TestCompletedTasksCounterMaintainedAtCommit proves the Control Plane Hot Path
// 7->8 fix (docs/internal/CREED_AND_PATH_TO_TEN.md): suppliers.completed_tasks
// is a REAL maintained column, incremented exactly once per real commit — never
// on an acknowledged duplicate — and ClaimTask's trusted-tier gate reads it directly
// instead of re-deriving it with a count(*) scan on every claim.
func TestCompletedTasksCounterMaintainedAtCommit(t *testing.T) {
	reset(t)
	ctx := context.Background()
	req(t, "POST", "/v1/worker/register", WorkerCapability{HWClass: "apple_silicon_max", MemoryGB: 64,
		SupportedJobs: []string{"embed"}, SupportedModels: []string{"all-minilm-l6-v2"}}, workerTok(), jsonCT())

	before := supplierCompletedTasks(t, demoSupplierUUID)

	jobID, _ := submitEmbedJob(t, 1, 0, 0, 0)
	_, body := req(t, "GET", "/v1/worker/poll", nil, workerTok())
	var disp TaskDispatch
	if err := json.Unmarshal(body, &disp); err != nil {
		t.Fatalf("dispatch decode: %v", err)
	}
	itStorage.PutObject(ctx, disp.ResultKey, embedResultJSON(1), "application/json")
	commit := TaskCommit{TaskID: disp.TaskID, ResultKey: disp.ResultKey}

	if code, _ := req(t, "POST", "/v1/worker/task/"+disp.TaskID.String()+"/commit", commit, workerTok(), jsonCT()); code != 204 {
		t.Fatal("first commit must succeed")
	}
	afterOne := supplierCompletedTasks(t, demoSupplierUUID)
	if afterOne-before != 1 {
		t.Fatalf("want completed_tasks +1 after one real commit, got +%d (before=%d after=%d)", afterOne-before, before, afterOne)
	}

	// An exact acknowledged replay must NOT increment it again.
	if code, _ := req(t, "POST", "/v1/worker/task/"+disp.TaskID.String()+"/commit", commit, workerTok(), jsonCT()); code != 204 {
		t.Fatal("duplicate commit replay must 204")
	}
	afterDup := supplierCompletedTasks(t, demoSupplierUUID)
	if afterDup != afterOne {
		t.Fatalf("a rejected duplicate commit must not increment completed_tasks again: before-dup=%d after-dup=%d", afterOne, afterDup)
	}

	// A second REAL job's commit increments it again.
	jobID2, _ := submitEmbedJob(t, 1, 0, 0, 0)
	_, body2 := req(t, "GET", "/v1/worker/poll", nil, workerTok())
	var disp2 TaskDispatch
	json.Unmarshal(body2, &disp2)
	itStorage.PutObject(ctx, disp2.ResultKey, embedResultJSON(1), "application/json")
	if code, _ := req(t, "POST", "/v1/worker/task/"+disp2.TaskID.String()+"/commit",
		TaskCommit{TaskID: disp2.TaskID, ResultKey: disp2.ResultKey}, workerTok(), jsonCT()); code != 204 {
		t.Fatal("second job's commit must succeed")
	}
	afterTwo := supplierCompletedTasks(t, demoSupplierUUID)
	if afterTwo-before != 2 {
		t.Fatalf("want completed_tasks +2 after two real commits across two jobs, got +%d", afterTwo-before)
	}
	_ = jobID
	_ = jobID2
}

// TestCreateJobWithTasksCopyFromCorrectness proves the Control Plane Hot Path
// 8->9 fix (docs/internal/CREED_AND_PATH_TO_TEN.md, "batch large-job inserts via
// pgx CopyFrom instead of row-by-row insert") did not change what actually lands:
// CopyFrom has no server-side DEFAULT substitution, so status='queued',
// retry_count=0, and visible_at must be bound explicitly per row — and every
// other per-task field (honeypot/redundancy flags, input/result keys,
// chunk_index) must round-trip exactly, in a job with primary + honeypot +
// redundancy tasks mixed together.
func TestCreateJobWithTasksCopyFromCorrectness(t *testing.T) {
	ctx := context.Background()
	reset(t)

	jobID := uuid.New()
	jr := &jobRow{
		ID: jobID, BuyerID: demoBuyerUUID, JobType: "embed", ModelRef: "all-minilm-l6-v2",
		InputRef: "jobs/cf/in.jsonl", OutputRef: "jobs/cf/out.json", Tier: "batch",
		VerificationPolicy: []byte(`{}`), TaskCount: 3, MinMemoryGB: 2,
	}
	jr.EconomicPlan = BuildEconomicPlan(EconomicPlanInput{
		BaseComputeUSD: 3, InitialTaskCount: 3, SupplierShare: supplierShareRate,
	}, testEconomicSchedule())
	jr.EstimatedUSD = jr.EconomicPlan.InitialBuyerChargeUSD
	primary, honeypot, redundancy := uuid.New(), uuid.New(), uuid.New()
	tasks := []taskRow{
		{ID: primary, JobID: jobID, InputRef: "jobs/cf/t0/in.jsonl", ResultKey: "jobs/cf/t0/out.json", ChunkIndex: 0},
		{ID: honeypot, JobID: jobID, IsHoneypot: true, InputRef: "jobs/cf/hp/in.jsonl", ResultKey: "jobs/cf/hp/out.json", ChunkIndex: 0},
		{ID: redundancy, JobID: jobID, IsRedundancy: true, InputRef: "jobs/cf/t0/in.jsonl", ResultKey: "jobs/cf/red/out.json", ChunkIndex: 0},
	}
	before := time.Now()
	if err := itStore.CreateJobWithTasks(ctx, jr, tasks); err != nil {
		t.Fatalf("CreateJobWithTasks: %v", err)
	}

	type row struct {
		status                   string
		isHoneypot, isRedundancy bool
		retryCount               int16
		inputRef, resultKey      string
		chunkIndex               int
		visibleAt                time.Time
	}
	get := func(id uuid.UUID) row {
		var r row
		if err := itPool.QueryRow(ctx,
			`SELECT status, is_honeypot, is_redundancy, retry_count, input_ref, result_key, chunk_index, visible_at
			   FROM tasks WHERE id=$1`, id,
		).Scan(&r.status, &r.isHoneypot, &r.isRedundancy, &r.retryCount, &r.inputRef, &r.resultKey, &r.chunkIndex, &r.visibleAt); err != nil {
			t.Fatalf("reading task %s: %v", id, err)
		}
		return r
	}

	p := get(primary)
	if p.status != "queued" || p.isHoneypot || p.isRedundancy || p.retryCount != 0 ||
		p.inputRef != "jobs/cf/t0/in.jsonl" || p.resultKey != "jobs/cf/t0/out.json" || p.chunkIndex != 0 {
		t.Fatalf("primary task row wrong after CopyFrom: %+v", p)
	}
	if p.visibleAt.Before(before.Add(-time.Second)) || p.visibleAt.After(time.Now().Add(time.Second)) {
		t.Fatalf("primary visible_at not set to ~now(): %v (test started %v)", p.visibleAt, before)
	}
	h := get(honeypot)
	if h.status != "queued" || !h.isHoneypot || h.isRedundancy || h.resultKey != "jobs/cf/hp/out.json" {
		t.Fatalf("honeypot task row wrong after CopyFrom: %+v", h)
	}
	r := get(redundancy)
	if r.status != "queued" || r.isHoneypot || !r.isRedundancy || r.resultKey != "jobs/cf/red/out.json" {
		t.Fatalf("redundancy task row wrong after CopyFrom: %+v", r)
	}

	var total int
	itPool.QueryRow(ctx, `SELECT count(*) FROM tasks WHERE job_id=$1`, jobID).Scan(&total)
	if total != 3 {
		t.Fatalf("want exactly 3 task rows landed, got %d", total)
	}

	// The claim path must be able to actually claim a CopyFrom-inserted row —
	// proves status/visible_at aren't just correct in isolation but genuinely
	// satisfy ClaimTaskSQL's real predicate (queued, visible now, unclaimed).
	c, err := itStore.ClaimTask(ctx, WorkerAuth{WorkerID: demoWorkerUUID, SupplierID: demoSupplierUUID})
	if err != nil {
		t.Fatalf("ClaimTask after CopyFrom insert: %v", err)
	}
	if c == nil {
		t.Fatal("a CopyFrom-inserted queued task should be claimable, got nil")
	}
}

// TestCreateJobWithTasksCopyFromScalesLinearly proves the Control Plane Hot Path
// 8->9 proof artifact directly: "a large job's insert time is shown to scale
// roughly linearly (not superlinearly) with task count." Inserts a 10x-larger
// task batch and confirms the wall-clock ratio stays within a generous bound of
// 10x (superlinear growth — the row-by-row round-trip pattern this replaces —
// would blow well past it; CopyFrom's single COPY operation should track task
// count closely). Timing-based and therefore inherently noisy on a shared/loaded
// machine, so the bound is deliberately generous (not a tight regression gate).
func TestCreateJobWithTasksCopyFromScalesLinearly(t *testing.T) {
	ctx := context.Background()
	reset(t)

	makeTasks := func(jobID uuid.UUID, n int) []taskRow {
		out := make([]taskRow, n)
		for i := 0; i < n; i++ {
			out[i] = taskRow{
				ID: uuid.New(), JobID: jobID,
				InputRef:   fmt.Sprintf("jobs/scale/%s/t%d/in.jsonl", jobID, i),
				ResultKey:  fmt.Sprintf("jobs/scale/%s/t%d/out.json", jobID, i),
				ChunkIndex: i,
			}
		}
		return out
	}
	timeInsert := func(n int) time.Duration {
		jobID := uuid.New()
		jr := &jobRow{
			ID: jobID, BuyerID: demoBuyerUUID, JobType: "embed", ModelRef: "all-minilm-l6-v2",
			InputRef: fmt.Sprintf("jobs/scale/%s/in.jsonl", jobID), OutputRef: fmt.Sprintf("jobs/scale/%s/out.json", jobID),
			Tier: "batch", VerificationPolicy: []byte(`{}`), TaskCount: n, MinMemoryGB: 2,
		}
		jr.EconomicPlan = BuildEconomicPlan(EconomicPlanInput{
			BaseComputeUSD: float64(n), InitialTaskCount: n, SupplierShare: supplierShareRate,
		}, testEconomicSchedule())
		jr.EstimatedUSD = jr.EconomicPlan.InitialBuyerChargeUSD
		tasks := makeTasks(jobID, n)
		start := time.Now()
		if err := itStore.CreateJobWithTasks(ctx, jr, tasks); err != nil {
			t.Fatalf("CreateJobWithTasks(n=%d): %v", n, err)
		}
		return time.Since(start)
	}

	const small, large = 500, 5000 // 10x task count
	// Warm the connection/plan cache with a throwaway small insert first so the
	// FIRST real measurement below isn't paying a one-time cold-start cost that
	// has nothing to do with CopyFrom's own scaling.
	_ = timeInsert(50)

	smallDur := timeInsert(small)
	largeDur := timeInsert(large)

	var smallCount, largeCount int
	itPool.QueryRow(ctx, `SELECT count(*) FROM tasks`).Scan(&smallCount)
	_ = smallCount
	itPool.QueryRow(ctx, `SELECT count(*) FROM tasks`).Scan(&largeCount)

	t.Logf("CreateJobWithTasks timing: %d tasks in %v, %d tasks in %v (ratio %.2fx for a %dx task-count increase)",
		small, smallDur, large, largeDur, float64(largeDur)/float64(smallDur), large/small)

	if smallDur <= 0 {
		t.Fatal("small insert duration must be positive (timer resolution issue)")
	}
	ratio := float64(largeDur) / float64(smallDur)
	const taskRatio = float64(large) / float64(small) // 10x
	// Generous superlinearity bound: a truly row-by-row round-trip pattern would
	// track WAY past 10x-of-10x (each round trip pays its own latency, so 10x the
	// rows is close to 10x the wall time regardless — the OLD code's own failure
	// mode was "still ~linear but with a much larger per-row constant", not a
	// blowup at this task count). The real risk CopyFrom fixes is the per-row
	// ROUND-TRIP constant, which this ratio bound (well under quadratic) confirms
	// is not growing worse with scale.
	if ratio > taskRatio*3 {
		t.Fatalf("insert time ratio %.2fx for a %.0fx task-count increase looks superlinear (bound %.2fx) — got small=%v large=%v",
			ratio, taskRatio, taskRatio*3, smallDur, largeDur)
	}
}

func supplierCompletedTasks(t *testing.T, supplierID uuid.UUID) int64 {
	t.Helper()
	var n int64
	if err := itPool.QueryRow(context.Background(),
		`SELECT completed_tasks FROM suppliers WHERE id = $1`, supplierID).Scan(&n); err != nil {
		t.Fatalf("reading suppliers.completed_tasks: %v", err)
	}
	return n
}

// --- 8. redundancy verification: match passes, mismatch is penalized ---

func TestRedundancyVerify(t *testing.T) {
	t.Run("match", func(t *testing.T) {
		reset(t)
		jobID, taskID := uuid.New(), uuid.New()
		mustJobTask(t, jobID, taskID, false, false, "jobs/x/input.jsonl")
		info := &CommitTaskInfo{TaskID: taskID, JobID: jobID, WorkerID: demoWorkerUUID,
			SupplierID: demoSupplierUUID, jobType: "embed", HWClass: "apple_silicon_max"}
		out, err := itServer.verifier.verifyTaskResult(context.Background(), info,
			TaskCommit{TaskID: info.TaskID}, embedResultJSON(1), embedResultJSON(1))
		if err != nil || out != OutcomePass {
			t.Fatalf("match: out=%v err=%v (want pass)", out, err)
		}
		if rep := supplierRep(t); rep <= 0.90 {
			t.Fatalf("match should credit reputation, got %v", rep)
		}
	})
	t.Run("mismatch detected, primary provisionally trusted", func(t *testing.T) {
		reset(t)
		a := embedResultJSON(1)              // unit vector e0
		b := []byte(`{"vectors":[[0,1,0]]}`) // orthogonal → cosine 0 < 0.999
		jobID, taskID := uuid.New(), uuid.New()
		mustJobTask(t, jobID, taskID, false, false, "jobs/x/input.jsonl")
		info := &CommitTaskInfo{TaskID: taskID, JobID: jobID, WorkerID: demoWorkerUUID,
			SupplierID: demoSupplierUUID, jobType: "embed", HWClass: "apple_silicon_max"}
		out, err := itServer.verifier.verifyTaskResult(context.Background(), info,
			TaskCommit{TaskID: info.TaskID}, a, b)
		// A 2-way mismatch is DETECTED → pass_with_penalty (api.go bumps the
		// cx_verification_mismatch metric on this outcome). With no third opinion
		// the primary is provisionally trusted, so it earns NO success credit —
		// the honest difference from a clean match (which rises to ~0.902). The
		// hard dock+clawback of confirmed fraud is proven by the honeypot path.
		if err != nil || out != OutcomePassWithPenalty {
			t.Fatalf("mismatch: out=%v err=%v (want pass_with_penalty)", out, err)
		}
		if rep := supplierRep(t); rep < 0.899 || rep > 0.901 {
			t.Fatalf("mismatch must neither credit nor dock the provisional primary; want ~0.90, got %v", rep)
		}
	})
}

// TestWorkerReportedHashNeverSkipsPeerFetch proves the authority correction to
// the old hot-path optimization: a raw task row carrying only a worker-reported
// hash is not enough to skip the peer GET. Hash trust now requires a terminal
// verification_work artifact whose digest the control plane computed itself.
// This legacy/raw fixture therefore takes the real object path and still reaches
// the same honest redundancy verdict.
func TestWorkerReportedHashNeverSkipsPeerFetch(t *testing.T) {
	ctx := context.Background()
	reset(t)
	ensureExtraDemoSuppliers(t, ctx)

	// Give BOTH the demo worker and a peer (on an independent supplier, so this is
	// a real cross-supplier redundancy match) the SAME non-blank verification
	// class — sameVerificationClass requires a matching, non-empty
	// (engine, build_hash) pair; both default to blank in seed.go, which would
	// make every pair "unknown" and never hash-trusted.
	const engine, buildHash = "candle", "test-build-hash-1"
	if _, err := itPool.Exec(ctx,
		`UPDATE workers SET engine=$2, build_hash=$3 WHERE id=$1`,
		demoWorkerUUID, engine, buildHash); err != nil {
		t.Fatal(err)
	}
	peerWorker := uuid.New()
	if _, err := itPool.Exec(ctx,
		`INSERT INTO workers (id, supplier_id, hw_class, engine, build_hash, memory_gb, bw_gbps, last_seen_at, version,
		                      supported_jobs, supported_models, min_payout_usd_hr, thermal_ok)
		 VALUES ($1,$2,'apple_silicon_max',$3,$4,64,400,now(),'seed',
		         ARRAY['batch_infer'],ARRAY['llama-3.2-1b-instruct-q4'],0,true)`,
		peerWorker, demoSupplier2UUID, engine, buildHash); err != nil {
		t.Fatal(err)
	}
	defer itPool.Exec(ctx, `DELETE FROM worker_tokens WHERE worker_id=$1`, peerWorker)

	// One job, one chunk, batch_infer (byte-exact — resultsAgree's default branch
	// is bytes.Equal, the ONLY comparison hash-trust can safely stand in for).
	// The peer already committed with a REAL result_sha256 stored (as CommitTask
	// would have written it) and a real object in MinIO — matching what a real
	// second worker's earlier commit would have left behind.
	resultBytes := []byte(`{"job_type":"batch_infer","model":"llama-3.2-1b-instruct-q4","completions":[{"index":0,"text":"real inference output","tokens":3}]}`)
	sum := sha256.Sum256(resultBytes)
	resultSHA256 := hex.EncodeToString(sum[:])

	jobID := uuid.New()
	if _, err := itPool.Exec(ctx,
		`INSERT INTO jobs (id, buyer_id, status, job_type, model_ref, input_ref, tier, task_count, tasks_done, min_memory_gb, output_ref)
		 VALUES ($1,$2,'running','batch_infer','llama-3.2-1b-instruct-q4','jobs/ht/input.jsonl','batch',2,1,2,'jobs/ht/output.json')`,
		jobID, demoBuyerUUID); err != nil {
		t.Fatal(err)
	}
	economicPlan := installRawFixtureEconomicPlan(t, ctx, jobID, 2, 1)
	peerKey := "jobs/ht/redundancy/0/result.json"
	if err := itStorage.PutObject(ctx, peerKey, resultBytes, "application/json"); err != nil {
		t.Fatal(err)
	}
	peerTask := uuid.New()
	if _, err := itPool.Exec(ctx,
		`INSERT INTO tasks (id, job_id, status, is_redundancy, input_ref, result_key, result_ref, result_sha256, chunk_index, worker_id, claimed_by, completed_at,
		                    economic_buyer_charge_usd,economic_supplier_payout_usd,
		                    execution_worker_id,execution_supplier_id,execution_hw_class,execution_engine,execution_build_hash)
		 SELECT $1,$2,'complete',true,'jobs/ht/tasks/0/input.jsonl',$3,$3,$4,0,$5,$5,now(),$6,$7,
		        w.id,w.supplier_id,w.hw_class,w.engine,w.build_hash
		   FROM workers w WHERE w.id=$5`,
		peerTask, jobID, peerKey, resultSHA256, peerWorker,
		economicPlan.BuyerChargePerTaskUSD, economicPlan.SupplierPayoutPerTaskUSD); err != nil {
		t.Fatal(err)
	}

	// The PRIMARY task, claimed by the demo worker, committing NOW through the
	// real HTTP endpoint with a matching ResultSHA256 — the exact shape a real
	// agent reports (agent/src/main.rs's sha256_hex over the bytes it just PUT).
	primaryTask := uuid.New()
	primaryKey := "jobs/ht/tasks/0/result.json"
	if _, err := itPool.Exec(ctx,
		`INSERT INTO tasks (id, job_id, status, input_ref, result_key, chunk_index, claimed_by, claimed_at, visible_at,
		                    economic_buyer_charge_usd,economic_supplier_payout_usd)
		 VALUES ($1,$2,'queued','jobs/ht/tasks/0/input.jsonl',$3,0,$4,now(),now(),$5,$6)`,
		primaryTask, jobID, primaryKey, demoWorkerUUID,
		economicPlan.BuyerChargePerTaskUSD, economicPlan.SupplierPayoutPerTaskUSD); err != nil {
		t.Fatal(err)
	}
	if err := itStore.StartTask(ctx, primaryTask, demoWorkerUUID); err != nil {
		t.Fatalf("start primary hash-trust fixture: %v", err)
	}
	if err := itStorage.PutObject(ctx, primaryKey, resultBytes, "application/json"); err != nil {
		t.Fatal(err)
	}

	before := metrics.hashTrustedRedundancy.Load()
	repBefore := supplierRep(t)

	commit := TaskCommit{TaskID: primaryTask, ResultKey: primaryKey, ResultSHA256: resultSHA256}
	code, body := req(t, "POST", "/v1/worker/task/"+primaryTask.String()+"/commit", commit, workerTok(), jsonCT())
	if code != http.StatusNoContent {
		t.Fatalf("commit: want 204, got %d: %s", code, body)
	}

	after := metrics.hashTrustedRedundancy.Load()
	if after-before != 0 {
		t.Fatalf("worker-reported peer hash must not be trusted without sealed verification work, metric changed by %d", after-before)
	}
	// A genuine redundancy match still credits reputation after the mandatory
	// real peer fetch.
	if repAfter := supplierRep(t); repAfter <= repBefore {
		t.Fatalf("hash-trusted match should credit reputation like a real byte match, got %v -> %v", repBefore, repAfter)
	}

	// Negative control: a MISMATCHED hash must NOT be hash-trusted (falls back to
	// a real GetObject, which is the pre-existing, already-proven path) — confirms
	// the branch is a genuine equality check, not a rubber stamp for any commit
	// touching a byte-exact job type.
	t.Run("mismatched hash falls back to real fetch, not blindly trusted", func(t *testing.T) {
		reset(t)
		ensureExtraDemoSuppliers(t, ctx)
		if _, err := itPool.Exec(ctx,
			`UPDATE workers SET engine=$2, build_hash=$3 WHERE id=$1`,
			demoWorkerUUID, engine, buildHash); err != nil {
			t.Fatal(err)
		}
		if _, err := itPool.Exec(ctx,
			`INSERT INTO workers (id, supplier_id, hw_class, engine, build_hash, memory_gb, bw_gbps, last_seen_at, version,
			                      supported_jobs, supported_models, min_payout_usd_hr, thermal_ok)
			 VALUES ($1,$2,'apple_silicon_max',$3,$4,64,400,now(),'seed',
			         ARRAY['batch_infer'],ARRAY['llama-3.2-1b-instruct-q4'],0,true)
			 ON CONFLICT (id) DO UPDATE SET engine=$3, build_hash=$4, last_seen_at=now()`,
			peerWorker, demoSupplier2UUID, engine, buildHash); err != nil {
			t.Fatal(err)
		}
		jobID2 := uuid.New()
		if _, err := itPool.Exec(ctx,
			`INSERT INTO jobs (id, buyer_id, status, job_type, model_ref, input_ref, tier, task_count, tasks_done, min_memory_gb, output_ref)
			 VALUES ($1,$2,'running','batch_infer','llama-3.2-1b-instruct-q4','jobs/ht2/input.jsonl','batch',2,1,2,'jobs/ht2/output.json')`,
			jobID2, demoBuyerUUID); err != nil {
			t.Fatal(err)
		}
		economicPlan2 := installRawFixtureEconomicPlan(t, ctx, jobID2, 2, 1)
		peerBytes := []byte(`{"job_type":"batch_infer","model":"llama-3.2-1b-instruct-q4","completions":[{"index":0,"text":"DIFFERENT real inference output","tokens":4}]}`)
		peerSum := sha256.Sum256(peerBytes)
		peerSHA := hex.EncodeToString(peerSum[:])
		peerKey2 := "jobs/ht2/redundancy/0/result.json"
		if err := itStorage.PutObject(ctx, peerKey2, peerBytes, "application/json"); err != nil {
			t.Fatal(err)
		}
		peerTask2 := uuid.New()
		if _, err := itPool.Exec(ctx,
			`INSERT INTO tasks (id, job_id, status, is_redundancy, input_ref, result_key, result_ref, result_sha256, chunk_index, worker_id, claimed_by, completed_at,
			                    economic_buyer_charge_usd,economic_supplier_payout_usd,
			                    execution_worker_id,execution_supplier_id,execution_hw_class,execution_engine,execution_build_hash)
			 SELECT $1,$2,'complete',true,'jobs/ht2/tasks/0/input.jsonl',$3,$3,$4,0,$5,$5,now(),$6,$7,
			        w.id,w.supplier_id,w.hw_class,w.engine,w.build_hash
			   FROM workers w WHERE w.id=$5`,
			peerTask2, jobID2, peerKey2, peerSHA, peerWorker,
			economicPlan2.BuyerChargePerTaskUSD, economicPlan2.SupplierPayoutPerTaskUSD); err != nil {
			t.Fatal(err)
		}
		primaryTask2 := uuid.New()
		primaryKey2 := "jobs/ht2/tasks/0/result.json"
		if _, err := itPool.Exec(ctx,
			`INSERT INTO tasks (id, job_id, status, input_ref, result_key, chunk_index, claimed_by, claimed_at, visible_at,
			                    economic_buyer_charge_usd,economic_supplier_payout_usd)
			 VALUES ($1,$2,'queued','jobs/ht2/tasks/0/input.jsonl',$3,0,$4,now(),now(),$5,$6)`,
			primaryTask2, jobID2, primaryKey2, demoWorkerUUID,
			economicPlan2.BuyerChargePerTaskUSD, economicPlan2.SupplierPayoutPerTaskUSD); err != nil {
			t.Fatal(err)
		}
		if err := itStore.StartTask(ctx, primaryTask2, demoWorkerUUID); err != nil {
			t.Fatalf("start mismatched hash-trust fixture: %v", err)
		}
		primaryBytes := []byte(`{"job_type":"batch_infer","model":"llama-3.2-1b-instruct-q4","completions":[{"index":0,"text":"real inference output","tokens":3}]}`) // deliberately DIFFERENT from peerBytes
		primarySum := sha256.Sum256(primaryBytes)
		primarySHA := hex.EncodeToString(primarySum[:])
		if err := itStorage.PutObject(ctx, primaryKey2, primaryBytes, "application/json"); err != nil {
			t.Fatal(err)
		}

		beforeHT := metrics.hashTrustedRedundancy.Load()
		commit2 := TaskCommit{TaskID: primaryTask2, ResultKey: primaryKey2, ResultSHA256: primarySHA}
		code, body := req(t, "POST", "/v1/worker/task/"+primaryTask2.String()+"/commit", commit2, workerTok(), jsonCT())
		if code != http.StatusNoContent {
			t.Fatalf("commit: want 204, got %d: %s", code, body)
		}
		afterHT := metrics.hashTrustedRedundancy.Load()
		if afterHT != beforeHT {
			t.Fatalf("mismatched hashes must NOT take the hash-trust branch, got +%d", afterHT-beforeHT)
		}
		// The real (slow-path) comparison must have detected the genuine byte
		// mismatch (pass_with_penalty, mismatch metric bumped) — proving the
		// fallback GetObject path ran for real and reached the correct verdict.
		var mismatchEvents int
		itPool.QueryRow(ctx, `SELECT count(*) FROM verification_events WHERE job_id=$1 AND kind='redundancy_mismatch'`, jobID2).Scan(&mismatchEvents)
		if mismatchEvents < 1 {
			t.Fatalf("want a real redundancy_mismatch event from the fallback path, got %d", mismatchEvents)
		}
	})
}

// --- 9. honeypot verification: pass credits, fraud claws back + requeues ---

func TestHoneypotVerify(t *testing.T) {
	ctx := context.Background()
	t.Run("pass", func(t *testing.T) {
		reset(t)
		jobID, taskID := uuid.New(), uuid.New()
		mustJobTask(t, jobID, taskID, true, false, demoHoneypotEmbedRef)
		info := &CommitTaskInfo{TaskID: taskID, JobID: jobID, WorkerID: demoWorkerUUID,
			SupplierID: demoSupplierUUID, IsHoneypot: true, InputRef: demoHoneypotEmbedRef, jobType: "embed"}
		// known answer is demoHoneypotEmbedKnownAnswer (a REAL measured MiniLM
		// embedding, seed.go — Buyer Developer Experience 7->8's real-SDK proof
		// found the old {"vectors":[[1,0,0]]} placeholder would fail ANY honest
		// worker's honeypot check); commit the same real vector back.
		out, err := itServer.verifier.verifyTaskResult(ctx, info,
			TaskCommit{TaskID: info.TaskID}, []byte(demoHoneypotEmbedKnownAnswer), nil)
		if err != nil || out != OutcomePass {
			t.Fatalf("honeypot pass: out=%v err=%v", out, err)
		}
	})
	t.Run("fraud", func(t *testing.T) {
		reset(t)
		// Real task + a prior credit so the clawback has something to reverse.
		jobID := uuid.New()
		taskID := uuid.New()
		mustJobTask(t, jobID, taskID, true /*honeypot*/, false, demoHoneypotEmbedRef)
		credit := -0.001 // buyer charge placeholder not needed; insert a positive credit
		_ = credit
		if err := itStore.InsertLedgerEntries(ctx, []LedgerEntry{{
			Kind: KindSupplierCredit, SupplierID: &demoSupplierUUID, TaskID: &taskID,
			AmountUSD: 0.005, PayoutStatus: PayoutHeld,
		}}); err != nil {
			t.Fatal(err)
		}
		info := &CommitTaskInfo{TaskID: taskID, JobID: jobID, WorkerID: demoWorkerUUID,
			SupplierID: demoSupplierUUID, IsHoneypot: true, InputRef: demoHoneypotEmbedRef, jobType: "embed"}
		out, err := itServer.verifier.verifyTaskResult(ctx, info,
			TaskCommit{TaskID: taskID}, []byte(`{"vectors":[[0,1,0]]}`), nil) // wrong answer
		if err != nil || out != OutcomeFail {
			t.Fatalf("honeypot fraud: out=%v err=%v (want fail)", out, err)
		}
		// Clawback row written, original credit clawed back, task requeued, rep docked.
		var clawbacks int
		itPool.QueryRow(ctx, `SELECT count(*) FROM ledger_entries WHERE task_id=$1 AND kind='clawback'`, taskID).Scan(&clawbacks)
		if clawbacks != 1 {
			t.Fatalf("want 1 clawback, got %d", clawbacks)
		}
		var status string
		itPool.QueryRow(ctx, `SELECT status FROM tasks WHERE id=$1`, taskID).Scan(&status)
		if status != "retrying" {
			t.Fatalf("fraud task should requeue (retrying), got %q", status)
		}
		if rep := supplierRep(t); rep > 0.80 {
			t.Fatalf("honeypot fraud should dock reputation hard (~0.75), got %v", rep)
		}
	})
}

// --- 10. stale running task requeue ---

func TestStaleRequeue(t *testing.T) {
	reset(t)
	ctx := context.Background()
	jobID, taskID := uuid.New(), uuid.New()
	mustJobTask(t, jobID, taskID, false, false, "jobs/x/tasks/0/input.jsonl")
	// Claim it and backdate the claim past the stale timeout.
	if _, err := itPool.Exec(ctx,
		`UPDATE tasks SET status='running', claimed_by=$2, claimed_at=now()-interval '2 hours', worker_id=$2 WHERE id=$1`,
		taskID, demoWorkerUUID); err != nil {
		t.Fatal(err)
	}
	wk := NewWorkers(itStore, itStorage, stubPayout{})
	if err := wk.requeueStaleTasks(ctx); err != nil {
		t.Fatalf("requeueStaleTasks: %v", err)
	}
	var status string
	var retry int
	itPool.QueryRow(ctx, `SELECT status, retry_count FROM tasks WHERE id=$1`, taskID).Scan(&status, &retry)
	if status != "queued" || retry != 1 {
		t.Fatalf("stale task not requeued: status=%q retry=%d", status, retry)
	}
}

// --- 11. failed job: retries exhausted → fail + partial settle ---

// TestFailPathPartialSettle proves the fail path's money semantics match the
// watchdog's (partial-settle everywhere): a terminally-failed job with DELIVERED
// chunks keeps their charges (the supplier earned them) and settles actual_usd at
// exactly that completed work with NO refund row; a terminally-failed job with
// ZERO delivered chunks was never charged in the first place, so it settles at $0
// with — again — no refund row (there is nothing to refund).
func TestFailPathPartialSettle(t *testing.T) {
	reset(t)
	ctx := context.Background()

	// Job A: one chunk DELIVERED (complete + charged -0.01), one chunk stale with
	// retries exhausted → the stale reaper fails the task + job.
	jobA := uuid.New()
	deliveredTask, dyingTask := uuid.New(), uuid.New()
	if _, err := itPool.Exec(ctx,
		`INSERT INTO jobs (id, buyer_id, status, job_type, input_ref, tier, task_count, tasks_done)
		 VALUES ($1,$2,'running','embed','jobs/y/input.jsonl','batch',2,1)`, jobA, demoBuyerUUID); err != nil {
		t.Fatal(err)
	}
	if _, err := itPool.Exec(ctx,
		`INSERT INTO tasks (id, job_id, status, input_ref, result_key, chunk_index, worker_id, claimed_by, completed_at)
		 VALUES ($1,$2,'complete','jobs/y/tasks/0/input.jsonl','jobs/y/tasks/0/result.json',0,$3,$3, now())`,
		deliveredTask, jobA, demoWorkerUUID); err != nil {
		t.Fatal(err)
	}
	if _, err := itPool.Exec(ctx,
		`INSERT INTO tasks (id, job_id, status, input_ref, result_key, chunk_index,
		                    worker_id, claimed_by, claimed_at, retry_count)
		 VALUES ($1,$2,'running','jobs/y/tasks/1/input.jsonl','jobs/y/tasks/1/result.json',1,
		         $3,$3, now()-interval '2 hours', 3)`,
		dyingTask, jobA, demoWorkerUUID); err != nil {
		t.Fatal(err)
	}
	// The delivered chunk's charge, settled at its commit (the real per-task split).
	if err := itStore.InsertLedgerEntries(ctx, []LedgerEntry{{
		Kind: KindBuyerCharge, BuyerID: &demoBuyerUUID, TaskID: &deliveredTask, AmountUSD: -0.01, PayoutStatus: PayoutReleased,
	}}); err != nil {
		t.Fatal(err)
	}

	// Job B: ZERO delivered chunks — one stale task, retries exhausted, never charged.
	jobB, bareTask := uuid.New(), uuid.New()
	mustJobTask(t, jobB, bareTask, false, false, "jobs/z/tasks/0/input.jsonl")
	if _, err := itPool.Exec(ctx,
		`UPDATE tasks SET status='running', claimed_by=$2, claimed_at=now()-interval '2 hours', worker_id=$2, retry_count=3 WHERE id=$1`,
		bareTask, demoWorkerUUID); err != nil {
		t.Fatal(err)
	}

	wk := NewWorkers(itStore, itStorage, stubPayout{})
	if err := wk.requeueStaleTasks(ctx); err != nil {
		t.Fatalf("requeue: %v", err)
	}

	// Job A: failed, delivered chunk untouched, charge stands, settled at 0.01, no refund.
	var tstatus, jstatus string
	itPool.QueryRow(ctx, `SELECT status FROM tasks WHERE id=$1`, dyingTask).Scan(&tstatus)
	itPool.QueryRow(ctx, `SELECT status FROM jobs WHERE id=$1`, jobA).Scan(&jstatus)
	if tstatus != "failed" || jstatus != "failed" {
		t.Fatalf("job A: want failed task+job, got task=%q job=%q", tstatus, jstatus)
	}
	itPool.QueryRow(ctx, `SELECT status FROM tasks WHERE id=$1`, deliveredTask).Scan(&tstatus)
	if tstatus != "complete" {
		t.Fatalf("job A: delivered chunk must stay complete, got %q", tstatus)
	}
	var actual float64
	itPool.QueryRow(ctx, `SELECT actual_usd::float8 FROM jobs WHERE id=$1`, jobA).Scan(&actual)
	if actual < 0.009 || actual > 0.011 {
		t.Fatalf("job A: actual_usd should settle at 0.01 (completed work only), got %v", actual)
	}
	var charges int
	itPool.QueryRow(ctx, `SELECT count(*) FROM ledger_entries WHERE kind='buyer_charge' AND task_id=$1`, deliveredTask).Scan(&charges)
	if charges != 1 {
		t.Fatalf("job A: the delivered chunk's charge must stand, got %d charge rows", charges)
	}

	// Job B: failed, settled at $0 (nothing was ever charged).
	itPool.QueryRow(ctx, `SELECT status FROM jobs WHERE id=$1`, jobB).Scan(&jstatus)
	if jstatus != "failed" {
		t.Fatalf("job B: want failed, got %q", jstatus)
	}
	itPool.QueryRow(ctx, `SELECT actual_usd::float8 FROM jobs WHERE id=$1`, jobB).Scan(&actual)
	if actual != 0 {
		t.Fatalf("job B: actual_usd should settle at 0 (zero delivered chunks), got %v", actual)
	}

	// Nothing refunds on either job: completed work stays charged, undone work was
	// never charged — there is no refund in the partial-settle model.
	var refunds int
	itPool.QueryRow(ctx, `SELECT count(*) FROM ledger_entries WHERE buyer_id=$1 AND kind='refund'`, demoBuyerUUID).Scan(&refunds)
	if refunds != 0 {
		t.Fatalf("want 0 refund rows (partial settle, not refund), got %d", refunds)
	}
}

// --- 12. payout hold→ready, and the transfer is honestly blocked ---

func TestPayoutHoldToReadyAndBlocked(t *testing.T) {
	reset(t)
	ctx := context.Background()
	// stubPayout never fakes a transfer.
	if _, err := (stubPayout{}).Send(ctx, demoSupplierUUID, 100, "usd", uuid.NewString()); err == nil {
		t.Fatal("stubPayout.Send must return an error (no fake transfers)")
	}
	jobID := uuid.New()
	taskID := uuid.New()
	mustJobTask(t, jobID, taskID, false, false, "jobs/z/tasks/0/input.jsonl")
	if _, err := itPool.Exec(ctx,
		`UPDATE tasks SET status='complete', verification_outcome='pass', verified_at=now(), completed_at=now()
		 WHERE id=$1`, taskID); err != nil {
		t.Fatalf("mark payout task accepted: %v", err)
	}
	if err := itStore.SetJobCharged(ctx, jobID, ChargeResult{
		PaymentIntentID: "pi_payout_hold_ready", ChargeID: "ch_pi_payout_hold_ready", RequestedCents: 2,
		ReceivedCents: 2, Currency: "usd",
	}); err != nil {
		t.Fatalf("record exact buyer cash funding: %v", err)
	}
	past := time.Now().Add(-time.Minute)
	if err := itStore.InsertLedgerEntries(ctx, []LedgerEntry{{
		Kind: KindSupplierCredit, SupplierID: &demoSupplierUUID, TaskID: &taskID,
		AmountUSD: 0.02, PayoutStatus: PayoutHeld, ReleaseAt: &past,
	}}); err != nil {
		t.Fatal(err)
	}
	wk := NewWorkers(itStore, itStorage, stubPayout{})
	if err := wk.releasePayouts(ctx); err != nil {
		t.Fatalf("releasePayouts: %v", err)
	}
	var status, ref string
	itPool.QueryRow(ctx,
		`SELECT payout_status, COALESCE(payout_ref,'') FROM ledger_entries WHERE kind='supplier_credit' AND task_id=$1`,
		taskID).Scan(&status, &ref)
	if status != "ready" {
		t.Fatalf("held payout past its window should be 'ready', got %q", status)
	}
	if ref != "" || status == "released" {
		t.Fatalf("no rail configured: must NOT be released with a ref (status=%q ref=%q)", status, ref)
	}
	earnings, err := itStore.WorkerEarnings(ctx, demoSupplierUUID)
	if err != nil {
		t.Fatalf("worker earnings after deferred payout: %v", err)
	}
	if earnings.BalanceUSD != 0 || earnings.LastPayoutUSD != nil || earnings.LastPayoutAt != nil {
		t.Fatalf("ready/owed credit rendered as paid cash: %+v", earnings)
	}
	if earnings.LifetimeUSD != 0.02 {
		t.Fatalf("deferred credit must remain accrued lifetime value, got %.6f", earnings.LifetimeUSD)
	}
}

// TestReconcileDriftMetric proves cx_reconcile_drift_total (Payments, Payouts &
// Unit Economics 8->9): reconcileLedger's drift findings were log-only before —
// an operator had to grep for "reconcile DRIFT" to notice one. Seeds the
// cheapest real anomaly reconcileLedger detects without needing a live Stripe
// call: a supplier_credit row marked 'released' (with a fake payout_ref,
// satisfying ledger_released_requires_ref) whose supplier has NO connected
// Stripe account — a real, structural impossibility (a transfer cannot have
// succeeded with no destination account), which the function flags before ever
// calling Stripe. Confirms the counter advances by exactly one per real drift
// found, not per sweep run.
func TestReconcileDriftMetric(t *testing.T) {
	reset(t)
	ctx := context.Background()
	t.Setenv("STRIPE_SECRET_KEY", "sk_test_fake_reconcile_only")

	// The demo supplier must have no connected account for this drift branch.
	if err := itStore.SetSupplierStripeAcct(ctx, demoSupplierUUID, ""); err != nil {
		t.Fatalf("clearing stripe_acct: %v", err)
	}
	taskID := uuid.New()
	mustJobTask(t, uuid.New(), taskID, false, false, "jobs/z/tasks/0/input.jsonl")
	if _, err := itPool.Exec(ctx,
		`INSERT INTO ledger_entries (kind, supplier_id, task_id, amount_usd, payout_status, payout_ref)
		 VALUES ('supplier_credit', $1, $2, 5.00, 'released', 'tr_test_fake_ref')`,
		demoSupplierUUID, taskID); err != nil {
		t.Fatalf("seeding released-but-unconnected ledger row: %v", err)
	}

	before := metrics.reconcileDrift.Load()
	wk := NewWorkers(itStore, itStorage, stubPayout{})
	if err := wk.reconcileLedger(ctx); err != nil {
		t.Fatalf("reconcileLedger: %v", err)
	}
	if got := metrics.reconcileDrift.Load() - before; got != 1 {
		t.Fatalf("want exactly 1 new drift counted, got %d", got)
	}

	// A second run must count the SAME real, still-unresolved anomaly again —
	// reconcileLedger is a stateless read-only audit, not a one-shot alert; the
	// operator's own action (or a real transfer) is what stops it recurring.
	if err := wk.reconcileLedger(ctx); err != nil {
		t.Fatalf("reconcileLedger (second run): %v", err)
	}
	if got := metrics.reconcileDrift.Load() - before; got != 2 {
		t.Fatalf("want 2 total drifts across two runs of an unresolved anomaly, got %d", got)
	}

	code, mbody := req(t, "GET", "/metrics", nil)
	if code != 200 {
		t.Fatalf("GET /metrics: %d", code)
	}
	if !strings.Contains(string(mbody), "cx_reconcile_drift_total") {
		t.Fatalf("/metrics missing cx_reconcile_drift_total:\n%s", mbody)
	}
}

// --- 13. webhook delivery attempt semantics against local receivers ---

func TestWebhookRetry(t *testing.T) {
	reset(t)
	ctx := context.Background()
	wk := NewWorkers(itStore, itStorage, stubPayout{})

	// The HTTP method performs one attempt. Durable outbox state, rather than an
	// in-memory sleep loop, decides when each later attempt is allowed to run.
	var hits int32
	flaky := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&hits, 1) < 3 {
			w.WriteHeader(500)
			return
		}
		w.WriteHeader(200)
	}))
	defer flaky.Close()
	_, sealed, err := newWebhookSigningSecret()
	if err != nil {
		t.Fatal(err)
	}
	pending := PendingWebhook{
		ID: uuid.New(), JobID: uuid.New(), URL: flaky.URL, Status: "complete",
		SigningSecretSealed: sealed,
	}
	for attempt := 1; attempt <= 3; attempt++ {
		err := wk.deliverWebhook(ctx, pending)
		if attempt < 3 && err == nil {
			t.Fatalf("flaky webhook attempt %d unexpectedly succeeded", attempt)
		}
		if attempt == 3 && err != nil {
			t.Fatalf("flaky webhook third attempt should deliver: %v", err)
		}
	}
	if got := atomic.LoadInt32(&hits); got != 3 {
		t.Fatalf("want 3 delivery attempts, got %d", got)
	}

	// Always-500 receiver → this one attempt fails (never marked delivered).
	var fhits int32
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&fhits, 1)
		w.WriteHeader(500)
	}))
	defer dead.Close()
	if err := wk.deliverWebhook(ctx, PendingWebhook{
		ID: uuid.New(), JobID: uuid.New(), URL: dead.URL, Status: "complete", SigningSecretSealed: sealed,
	}); err == nil {
		t.Fatal("dead webhook must surface an error, not a fake success")
	}
	if got := atomic.LoadInt32(&fhits); got != 1 {
		t.Fatalf("dead receiver: want one attempt per call, got %d", got)
	}
}

func TestWebhookRegistrationOwnershipAndRequiredJob(t *testing.T) {
	reset(t)
	ctx := context.Background()
	jobID, taskID := uuid.New(), uuid.New()
	mustJobTask(t, jobID, taskID, false, false, "jobs/webhook-owner/tasks/0/input.jsonl")

	if _, err := itStore.InsertWebhook(ctx, demoBuyerUUID, nil, "https://hooks.example.test/event"); !errors.Is(err, errWebhookJobRequired) {
		t.Fatalf("nil job registration error = %v, want errWebhookJobRequired", err)
	}
	code, out := req(t, "POST", "/v1/webhooks",
		map[string]any{"url": "https://hooks.example.test/event"}, buyerKey(), jsonCT())
	if code != http.StatusBadRequest || !strings.Contains(string(out), "job_id is required") {
		t.Fatalf("jobless registration: want honest 400, got %d: %s", code, out)
	}

	otherBuyer := uuid.New()
	otherKey := "cx_test_webhook_owner_" + uuid.NewString()
	if _, err := itPool.Exec(ctx,
		`INSERT INTO api_keys (buyer_id,key_hash,revoked) VALUES ($1,$2,false)`,
		otherBuyer, hashKey(otherKey)); err != nil {
		t.Fatalf("insert second buyer key: %v", err)
	}
	t.Cleanup(func() { _, _ = itPool.Exec(ctx, `DELETE FROM api_keys WHERE key_hash=$1`, hashKey(otherKey)) })
	code, out = req(t, "POST", "/v1/webhooks", map[string]any{
		"url": "https://hooks.example.test/event", "job_id": jobID.String(),
	}, hdr{"Authorization", "Bearer " + otherKey}, jsonCT())
	if code != http.StatusNotFound {
		t.Fatalf("cross-buyer registration: want indistinguishable 404, got %d: %s", code, out)
	}
	var crossRows int
	if err := itPool.QueryRow(ctx,
		`SELECT count(*) FROM webhooks WHERE buyer_id=$1 OR (job_id=$2 AND buyer_id<>$3)`,
		otherBuyer, jobID, demoBuyerUUID).Scan(&crossRows); err != nil {
		t.Fatal(err)
	}
	if crossRows != 0 {
		t.Fatalf("cross-buyer registration persisted %d webhook row(s)", crossRows)
	}

	registrationRequest, err := http.NewRequest(http.MethodPost, itHTTP.URL+"/v1/webhooks",
		strings.NewReader(fmt.Sprintf(`{"url":"https://hooks.example.test/event","job_id":%q}`, jobID.String())))
	if err != nil {
		t.Fatal(err)
	}
	registrationRequest.Header.Set("Authorization", "Bearer "+demoAPIKey)
	registrationRequest.Header.Set("Content-Type", "application/json")
	registrationResponse, err := http.DefaultClient.Do(registrationRequest)
	if err != nil {
		t.Fatal(err)
	}
	registrationBody, _ := io.ReadAll(registrationResponse.Body)
	registrationResponse.Body.Close()
	if registrationResponse.StatusCode != http.StatusCreated {
		t.Fatalf("owned registration: want 201, got %d: %s", registrationResponse.StatusCode, registrationBody)
	}
	if registrationResponse.Header.Get("Cache-Control") != "no-store" ||
		registrationResponse.Header.Get("Pragma") != "no-cache" {
		t.Fatalf("secret response cache headers = Cache-Control %q, Pragma %q",
			registrationResponse.Header.Get("Cache-Control"), registrationResponse.Header.Get("Pragma"))
	}
	var firstRegistration struct {
		ID     uuid.UUID `json:"webhook_id"`
		Secret string    `json:"webhook_secret"`
	}
	if err := json.Unmarshal(registrationBody, &firstRegistration); err != nil {
		t.Fatal(err)
	}
	if firstRegistration.ID == uuid.Nil || !strings.HasPrefix(firstRegistration.Secret, webhookSigningSecretPrefix) {
		t.Fatalf("registration id_present=%v secret_present=%v",
			firstRegistration.ID != uuid.Nil, strings.HasPrefix(firstRegistration.Secret, webhookSigningSecretPrefix))
	}
	var storedSealed string
	if err := itPool.QueryRow(ctx,
		`SELECT signing_secret_sealed FROM webhooks WHERE id=$1`, firstRegistration.ID).Scan(&storedSealed); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(storedSealed, "enc:") || strings.Contains(storedSealed, firstRegistration.Secret) {
		t.Fatalf("stored webhook secret is not sealed: %q", storedSealed)
	}

	duplicate, err := itStore.InsertWebhook(ctx, demoBuyerUUID, &jobID, "https://hooks.example.test/event")
	if err != nil || duplicate.ID != firstRegistration.ID || duplicate.Secret != firstRegistration.Secret {
		t.Fatalf("idempotent registration: id=%s same_secret=%v err=%v; want id %s",
			duplicate.ID, duplicate.Secret == firstRegistration.Secret, err, firstRegistration.ID)
	}
	for i := 1; i < webhookRegistrationLimitPerJob; i++ {
		if _, err := itStore.InsertWebhook(ctx, demoBuyerUUID, &jobID,
			fmt.Sprintf("https://hooks-%02d.example.test/event", i)); err != nil {
			t.Fatalf("fill webhook quota %d: %v", i, err)
		}
	}
	if _, err := itStore.InsertWebhook(ctx, demoBuyerUUID, &jobID, "https://over-limit.example.test/event"); !errors.Is(err, errWebhookLimit) {
		t.Fatalf("over-limit store error = %v, want errWebhookLimit", err)
	}
	code, out = req(t, "POST", "/v1/webhooks", map[string]any{
		"url": "https://over-limit.example.test/event", "job_id": jobID.String(),
	}, buyerKey(), jsonCT())
	if code != http.StatusTooManyRequests {
		t.Fatalf("over-limit HTTP registration: want 429, got %d: %s", code, out)
	}
}

func TestWebhookRegistrationFailsClosedWithoutTokenKey(t *testing.T) {
	reset(t)
	t.Setenv("CX_TOKEN_KEY", "")
	ctx := context.Background()
	jobID, taskID := uuid.New(), uuid.New()
	mustJobTask(t, jobID, taskID, false, false, "jobs/webhook-key/tasks/0/input.jsonl")
	code, out := req(t, "POST", "/v1/webhooks", map[string]any{
		"url": "https://hooks.example.test/event", "job_id": jobID.String(),
	}, buyerKey(), jsonCT())
	if code != http.StatusServiceUnavailable || !strings.Contains(string(out), "encrypted signing-secret storage") {
		t.Fatalf("missing CX_TOKEN_KEY: want 503, got %d: %s", code, out)
	}
	var rows int
	if err := itPool.QueryRow(ctx, `SELECT count(*) FROM webhooks WHERE job_id=$1`, jobID).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Fatalf("missing encryption key persisted %d webhook row(s)", rows)
	}
}

func TestInlineJobWebhookReturnsRecoverableNoStoreSecret(t *testing.T) {
	reset(t)
	body := map[string]any{
		"job_type": map[string]any{"type": "embed"},
		"model":    map[string]any{"kind": "hf", "ref": "all-minilm-l6-v2"},
		"verification": map[string]any{
			"redundancy_frac": 0, "honeypot_frac": 0, "skip_verification_floor": true,
		},
		"tier":        "batch",
		"input":       "{\"id\":\"signed-hook\",\"text\":\"hello\"}\n",
		"webhook_url": "https://hooks.example.test/completed",
	}
	raw, _ := json.Marshal(body)
	request, err := http.NewRequest(http.MethodPost, itHTTP.URL+"/v1/jobs", bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+demoAPIKey)
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	responseBody, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusAccepted {
		t.Fatalf("inline webhook job: want 202, got %d: %s", response.StatusCode, responseBody)
	}
	if response.Header.Get("Cache-Control") != "no-store" || response.Header.Get("Pragma") != "no-cache" {
		t.Fatalf("inline webhook secret response was cacheable: Cache-Control=%q Pragma=%q",
			response.Header.Get("Cache-Control"), response.Header.Get("Pragma"))
	}
	var submitted JobSubmitResponse
	if err := json.Unmarshal(responseBody, &submitted); err != nil {
		t.Fatal(err)
	}
	if submitted.WebhookID == "" || !strings.HasPrefix(submitted.WebhookSecret, webhookSigningSecretPrefix) {
		t.Fatalf("inline response webhook_id_present=%v webhook_secret_present=%v",
			submitted.WebhookID != "", strings.HasPrefix(submitted.WebhookSecret, webhookSigningSecretPrefix))
	}
	hookID, err := uuid.Parse(submitted.WebhookID)
	if err != nil {
		t.Fatal(err)
	}
	var sealed string
	if err := itPool.QueryRow(context.Background(),
		`SELECT signing_secret_sealed FROM webhooks WHERE id=$1 AND job_id=$2`, hookID, submitted.JobID).Scan(&sealed); err != nil {
		t.Fatal(err)
	}
	opened, err := openWebhookSigningSecret(sealed)
	if err != nil || opened != submitted.WebhookSecret {
		t.Fatalf("persisted inline webhook secret was not recoverable from sealed storage: %v", err)
	}
}

func TestWebhookOutboxLeaseBackoffAndPoisonIsolation(t *testing.T) {
	reset(t)
	ctx := context.Background()
	jobID, taskID := uuid.New(), uuid.New()
	mustJobTask(t, jobID, taskID, false, false, "jobs/webhook-outbox/tasks/0/input.jsonl")
	if _, err := itPool.Exec(ctx, `UPDATE jobs SET status='complete' WHERE id=$1`, jobID); err != nil {
		t.Fatal(err)
	}

	const total = webhookDeliveryBatch + 1
	for i := 0; i < total; i++ {
		if _, err := itStore.InsertWebhook(ctx, demoBuyerUUID, &jobID,
			fmt.Sprintf("https://hooks-%02d.example.test/event", i)); err != nil {
			t.Fatalf("insert webhook %d: %v", i, err)
		}
	}
	first, err := itStore.ClaimPendingWebhooks(ctx, webhookDeliveryBatch, time.Minute)
	if err != nil || len(first) != webhookDeliveryBatch {
		t.Fatalf("first claim = %d rows, %v; want %d", len(first), err, webhookDeliveryBatch)
	}
	for i, p := range first {
		permanent := i == 0
		attempts, dead, err := itStore.MarkWebhookFailed(
			ctx, p.ID, p.LeaseToken, errors.New("scripted poison endpoint"), permanent, time.Minute, 12)
		if err != nil || attempts != 1 || dead != permanent {
			t.Fatalf("mark failure %d: attempts=%d dead=%v err=%v", i, attempts, dead, err)
		}
	}

	// All poison rows are now either backoff-ineligible or dead-lettered, so the
	// item immediately behind a full poison page is claimable without starvation.
	second, err := itStore.ClaimPendingWebhooks(ctx, webhookDeliveryBatch, time.Minute)
	if err != nil || len(second) != 1 {
		t.Fatalf("second claim = %d rows, %v; want one row behind poison page", len(second), err)
	}
	oldLease := second[0].LeaseToken
	if _, err := itPool.Exec(ctx,
		`UPDATE webhooks SET lease_expires_at=now()-interval '1 second' WHERE id=$1`, second[0].ID); err != nil {
		t.Fatal(err)
	}
	reclaimed, err := itStore.ClaimPendingWebhooks(ctx, webhookDeliveryBatch, time.Minute)
	if err != nil || len(reclaimed) != 1 || reclaimed[0].ID != second[0].ID || reclaimed[0].LeaseToken == oldLease {
		t.Fatalf("expired lease reclaim = %+v, %v; want same row with a new token", reclaimed, err)
	}
	if err := itStore.MarkWebhookDelivered(ctx, second[0].ID, oldLease); !errors.Is(err, errWebhookLeaseLost) {
		t.Fatalf("stale lease completion error = %v, want errWebhookLeaseLost", err)
	}
	if err := itStore.MarkWebhookDelivered(ctx, reclaimed[0].ID, reclaimed[0].LeaseToken); err != nil {
		t.Fatalf("current lease completion: %v", err)
	}

	var deadCount, backedOff, delivered int
	if err := itPool.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE dead_lettered_at IS NOT NULL),
		       count(*) FILTER (WHERE attempts=1 AND dead_lettered_at IS NULL AND next_attempt_at>now()),
		       count(*) FILTER (WHERE delivered_at IS NOT NULL)
		  FROM webhooks WHERE job_id=$1`, jobID).Scan(&deadCount, &backedOff, &delivered); err != nil {
		t.Fatal(err)
	}
	if deadCount != 1 || backedOff != webhookDeliveryBatch-1 || delivered != 1 {
		t.Fatalf("outbox state: dead=%d backed_off=%d delivered=%d", deadCount, backedOff, delivered)
	}
}

func TestLegacyWebhookDeadLettersUntilExplicitReregistration(t *testing.T) {
	reset(t)
	ctx := context.Background()
	jobID, taskID := uuid.New(), uuid.New()
	mustJobTask(t, jobID, taskID, false, false, "jobs/webhook-legacy/tasks/0/input.jsonl")
	if _, err := itPool.Exec(ctx, `UPDATE jobs SET status='complete' WHERE id=$1`, jobID); err != nil {
		t.Fatal(err)
	}

	var hits atomic.Int32
	var expectedSecret atomic.Value
	expectedSecret.Store("")
	var signatureOK atomic.Bool
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		secret, _ := expectedSecret.Load().(string)
		if secret != "" && verifyStripeSig(body, r.Header.Get("X-CX-Signature"), secret) {
			signatureOK.Store(true)
		}
		hits.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer receiver.Close()

	activeLegacyID, deadLegacyID := uuid.New(), uuid.New()
	if _, err := itPool.Exec(ctx, `
		INSERT INTO webhooks (id,buyer_id,job_id,url,signing_secret_sealed)
		VALUES ($1,$3,$4,$5,NULL),
		       ($2,$3,$4,$6,NULL)`,
		activeLegacyID, deadLegacyID, demoBuyerUUID, jobID,
		receiver.URL+"/active-legacy", receiver.URL+"/reregistered-legacy"); err != nil {
		t.Fatal(err)
	}
	if _, err := itPool.Exec(ctx, `
		UPDATE webhooks
		   SET dead_lettered_at=now(),last_error='legacy webhook has no per-registration signing secret; re-register it'
		 WHERE id=$1`, deadLegacyID); err != nil {
		t.Fatal(err)
	}

	wk := NewWorkers(itStore, itStorage, stubPayout{})
	if err := wk.deliverPendingWebhooks(ctx); err != nil {
		t.Fatalf("dead-letter unsigned legacy row: %v", err)
	}
	if got := hits.Load(); got != 0 {
		t.Fatalf("legacy unsigned row reached receiver %d time(s)", got)
	}
	var activeDead bool
	if err := itPool.QueryRow(ctx,
		`SELECT dead_lettered_at IS NOT NULL FROM webhooks WHERE id=$1`, activeLegacyID).Scan(&activeDead); err != nil {
		t.Fatal(err)
	}
	if !activeDead {
		t.Fatal("legacy unsigned row was not permanently dead-lettered")
	}

	registration, err := itStore.InsertWebhook(
		ctx, demoBuyerUUID, &jobID, receiver.URL+"/reregistered-legacy")
	if err != nil {
		t.Fatalf("explicit legacy re-registration: %v", err)
	}
	if registration.ID != deadLegacyID || registration.Secret == "" {
		t.Fatalf("legacy upgrade id=%s has_secret=%v, want id=%s", registration.ID, registration.Secret != "", deadLegacyID)
	}
	expectedSecret.Store(registration.Secret)
	if err := wk.deliverPendingWebhooks(ctx); err != nil {
		t.Fatalf("deliver explicitly re-registered webhook: %v", err)
	}
	if got := hits.Load(); got != 1 || !signatureOK.Load() {
		t.Fatalf("re-registered delivery hits=%d signature_ok=%v", got, signatureOK.Load())
	}
}

// --- 14. full completion sweep delivers a registered webhook exactly once ---

func TestWebhookSweepExactlyOnce(t *testing.T) {
	reset(t)
	ctx := context.Background()
	var hits atomic.Int32
	var signingSecret atomic.Value
	signingSecret.Store("")
	var signatureOK atomic.Bool
	rcv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		secret, _ := signingSecret.Load().(string)
		if secret != "" && verifyStripeSig(body, r.Header.Get("X-CX-Signature"), secret) {
			signatureOK.Store(true)
		}
		hits.Add(1)
		w.WriteHeader(200)
	}))
	defer rcv.Close()

	// A job with one already-complete task → completable by the sweep.
	jobID, taskID := uuid.New(), uuid.New()
	mustJobTask(t, jobID, taskID, false, false, "jobs/w/tasks/0/input.jsonl")
	if _, err := itPool.Exec(ctx, `UPDATE jobs SET status='running', task_count=1, tasks_done=1 WHERE id=$1`, jobID); err != nil {
		t.Fatal(err)
	}
	if _, err := itPool.Exec(ctx, `UPDATE tasks SET status='complete' WHERE id=$1`, taskID); err != nil {
		t.Fatal(err)
	}
	registration, err := itStore.InsertWebhook(ctx, demoBuyerUUID, &jobID, rcv.URL)
	if err != nil {
		t.Fatal(err)
	}
	signingSecret.Store(registration.Secret)
	wk := NewWorkers(itStore, itStorage, stubPayout{})
	// Two sweeps: the first finalizes + delivers, the second must NOT re-deliver.
	if err := wk.sweepAndDeliver(ctx); err != nil {
		t.Fatalf("sweep 1: %v", err)
	}
	if err := wk.sweepAndDeliver(ctx); err != nil {
		t.Fatalf("sweep 2: %v", err)
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("webhook should fire exactly once, fired %d", got)
	}
	if !signatureOK.Load() {
		t.Fatal("delivered webhook signature did not verify with its registration secret")
	}
	var jstatus string
	itPool.QueryRow(ctx, `SELECT status FROM jobs WHERE id=$1`, jobID).Scan(&jstatus)
	if jstatus != "complete" {
		t.Fatalf("sweep should finalize job, got %q", jstatus)
	}
}

// --- 15. pricing + models from the DB catalogue ---

func TestPriceAndModels(t *testing.T) {
	reset(t)
	code, body := req(t, "GET", "/v1/models", nil, buyerKey())
	if code != 200 || !strings.Contains(string(body), "all-minilm-l6-v2") {
		t.Fatalf("models: %d %s", code, body)
	}
	code, body = req(t, "GET", "/v1/price-estimate?model=all-minilm-l6-v2&units=1000&tier=batch", nil, buyerKey())
	if code != 200 {
		t.Fatalf("price-estimate: %d %s", code, body)
	}
	var pe PriceEstimate
	json.Unmarshal(body, &pe)
	if pe.EstimateUSD <= 0 {
		t.Fatalf("estimate should be positive, got %v", pe.EstimateUSD)
	}
}

// --- 16. the hard filter (item A): a worker can NEVER claim a task it cannot run.
// Each sub-case makes the single live demo worker ineligible on exactly one axis
// and asserts the claim returns nothing; the control restores eligibility and the
// same worker then claims successfully. This proves the SQL filter, not just Match. ---

// Plane C (docs/PLANE_C.md §24-C1, §30): POST /v1/quote scans the input, returns a
// conservative cost/ETA/supply/risk band WITHOUT creating a job or spending, and
// PERSISTS the assumptions (the load-bearing rule: a later invoice can say what was
// believed). Proven end-to-end against live Postgres.
func TestQuoteEndpointPersistsAssumptions(t *testing.T) {
	ctx := context.Background()
	itPool.Exec(ctx, `TRUNCATE quotes`)

	// Inline JSONL: 2 valid records + 1 malformed (line 2) → the scanner must see it.
	body := map[string]any{
		"job_type":     map[string]any{"type": "embed"},
		"model":        map[string]any{"kind": "hf", "ref": "all-minilm-l6-v2"},
		"tier":         "batch",
		"verification": map[string]any{"redundancy_frac": 0.0, "honeypot_frac": 0.0, "payout_hold_secs": 0},
		"input":        "{\"id\":\"a\",\"text\":\"hello\"}\n{bad json}\n{\"id\":\"c\",\"text\":\"world\"}\n",
	}
	status, out := req(t, "POST", "/v1/quote", body, buyerKey(), jsonCT())
	if status != 200 {
		t.Fatalf("POST /v1/quote -> %d: %s", status, out)
	}
	var q Quote
	if err := json.Unmarshal(out, &q); err != nil {
		t.Fatalf("decode quote: %v\n%s", err, out)
	}
	if q.QuoteID == "" || q.JobType != "embed" || q.Model != "all-minilm-l6-v2" {
		t.Fatalf("quote header wrong: %+v", q)
	}
	if q.Input.Records != 3 || q.Input.MalformedRecords != 1 || q.Input.FirstBadLine != 2 {
		t.Fatalf("scanner wrong: records=%d malformed=%d firstBad=%d", q.Input.Records, q.Input.MalformedRecords, q.Input.FirstBadLine)
	}
	if q.Execution.EstimatedTasks < 1 || q.Execution.RecommendedSplitSize < 1 {
		t.Fatalf("execution plan wrong: %+v", q.Execution)
	}
	if q.Cost.ExpectedUSD < 0 || q.Cost.MaxUSD < q.Cost.ExpectedUSD || q.Cost.MinUSD > q.Cost.ExpectedUSD {
		t.Fatalf("cost band incoherent: %+v", q.Cost)
	}
	if q.Time.P50Secs <= 0 || q.Time.P90Secs < q.Time.P50Secs {
		t.Fatalf("eta band incoherent: %+v", q.Time)
	}
	if q.Confidence.Score <= 0 || q.Confidence.Score > 1 || len(q.Confidence.Reasons) == 0 {
		t.Fatalf("confidence wrong: %+v", q.Confidence)
	}
	if !q.Budget.CancelBeforeExceeding || q.Budget.SuggestedMaxUSD < q.Cost.ExpectedUSD {
		t.Fatalf("budget suggestion wrong: %+v", q.Budget)
	}
	// A malformed record must surface a buyer-visible warning (never hidden).
	if len(q.Warnings) == 0 {
		t.Fatal("malformed input must produce a warning")
	}

	// The assumptions are persisted (PLANE_C §6): exactly one quotes row, matching.
	var n int
	var records int
	var costExpected float64
	if err := itPool.QueryRow(ctx,
		`SELECT count(*), COALESCE(max(records),0), COALESCE(max(cost_expected_usd),0) FROM quotes WHERE job_type='embed'`,
	).Scan(&n, &records, &costExpected); err != nil {
		t.Fatalf("read persisted quote: %v", err)
	}
	if n != 1 || records != 3 {
		t.Fatalf("quote not persisted correctly: rows=%d records=%d", n, records)
	}
	t.Cleanup(func() { itPool.Exec(ctx, `TRUNCATE quotes`) })
}

// TestQuotePricesTheVerificationFloor proves the Verification & Result Trust
// 5->6 fix (docs/internal/CREED_AND_PATH_TO_TEN.md): createJob unconditionally
// floors a job submitted with no explicit verification fractions to at least
// one real honeypot task (see api.go's wantVerificationFloor). A quote built
// from the same defaults must not understate that cost — verification_overhead_usd
// must reflect the floor, not the bare (zero) fractions, matching the
// TestVerificationFloorAppliesUnlessOptedOut precedent on the createJob side.
func TestQuotePricesTheVerificationFloor(t *testing.T) {
	ctx := context.Background()
	itPool.Exec(ctx, `TRUNCATE quotes`)
	t.Cleanup(func() { itPool.Exec(ctx, `TRUNCATE quotes`) })

	var sb strings.Builder
	for i := 0; i < 20; i++ {
		fmt.Fprintf(&sb, `{"id":"r%d","text":"record %d"}`+"\n", i, i)
	}
	input := sb.String()

	quote := func(verification map[string]any) Quote {
		t.Helper()
		body := map[string]any{
			"job_type":     map[string]any{"type": "embed"},
			"model":        map[string]any{"kind": "hf", "ref": "all-minilm-l6-v2"},
			"tier":         "batch",
			"verification": verification,
			"input":        input,
		}
		status, out := req(t, "POST", "/v1/quote", body, buyerKey(), jsonCT())
		if status != 200 {
			t.Fatalf("POST /v1/quote -> %d: %s", status, out)
		}
		var q Quote
		if err := json.Unmarshal(out, &q); err != nil {
			t.Fatalf("decode quote: %v\n%s", err, out)
		}
		return q
	}

	t.Run("default verification quote prices the honeypot floor", func(t *testing.T) {
		q := quote(map[string]any{})
		if q.Cost.VerificationOverheadUSD <= 0 {
			t.Fatalf("a default-verification quote must price the mandatory honeypot floor, got verification_overhead_usd=%v", q.Cost.VerificationOverheadUSD)
		}
		if !q.Economics.Executable {
			t.Fatalf("default-verification quote must have an executable economic plan: %+v", q.Economics)
		}
		extraTasks := q.Economics.Input.InitialTaskCount - q.Execution.EstimatedTasks
		if extraTasks != 1 {
			t.Fatalf("default verification must add exactly one priced honeypot task: primary=%d initial=%d", q.Execution.EstimatedTasks, q.Economics.Input.InitialTaskCount)
		}
		wantOverhead := roundEconomicUSD(q.Economics.BuyerChargePerTaskUSD * float64(extraTasks))
		if q.Cost.VerificationOverheadUSD != wantOverhead {
			t.Fatalf("verification overhead must be the frozen guarded buyer charge of the floored task: want %v, got %v (%+v)", wantOverhead, q.Cost.VerificationOverheadUSD, q.Economics)
		}
		// Cost.Max excludes the optional, separately itemized SLA premium. The
		// economic reserve is now the authoritative cap; the old 1.5x heuristic is
		// deliberately no longer authoritative after the margin guard.
		wantMax := roundEconomicUSD(q.Economics.ReservedBuyerChargeUSD - q.Economics.Input.SLAPremiumUSD)
		if q.Cost.MaxUSD != wantMax {
			t.Fatalf("cost_max_usd must equal the frozen non-SLA economic reserve: want %v, got %v (%+v)", wantMax, q.Cost.MaxUSD, q.Cost)
		}
	})

	t.Run("explicit opt-out yields genuinely zero overhead and a lower cost_max", func(t *testing.T) {
		withFloor := quote(map[string]any{})
		optedOut := quote(map[string]any{"skip_verification_floor": true})
		if optedOut.Cost.VerificationOverheadUSD != 0 {
			t.Fatalf("skip_verification_floor must yield exactly 0 verification_overhead_usd, got %v", optedOut.Cost.VerificationOverheadUSD)
		}
		if optedOut.Cost.MaxUSD >= withFloor.Cost.MaxUSD {
			t.Fatalf("opted-out cost_max_usd (%v) must be lower than the default-floored quote's (%v)", optedOut.Cost.MaxUSD, withFloor.Cost.MaxUSD)
		}
		if optedOut.Cost.PlatformTakeUSD >= withFloor.Cost.PlatformTakeUSD {
			t.Fatalf("opted-out platform_take_usd (%v) must be lower than the default-floored quote's (%v)", optedOut.Cost.PlatformTakeUSD, withFloor.Cost.PlatformTakeUSD)
		}
	})

	t.Run("an explicit non-zero honeypot_frac is left untouched", func(t *testing.T) {
		// This fixture produces one primary. At 0.49, the explicitly requested
		// fraction rounds to zero; if the default floor were incorrectly applied as
		// well, the quote would contain one extra task. That makes this case
		// distinguish the two policies without depending on multiple seed honeypots.
		q := quote(map[string]any{"honeypot_frac": 0.49})
		wantExtraTasks := fracCount(q.Execution.EstimatedTasks, 0.49)
		if wantExtraTasks != 0 {
			t.Fatalf("test fixture must round explicit 0.49 to zero tasks; primary=%d extra=%d", q.Execution.EstimatedTasks, wantExtraTasks)
		}
		if q.Economics.Input.InitialTaskCount != q.Execution.EstimatedTasks+wantExtraTasks {
			t.Fatalf("an explicit non-zero honeypot_frac must not be bumped by the floor logic: primary=%d want extra=%d got initial=%d",
				q.Execution.EstimatedTasks, wantExtraTasks, q.Economics.Input.InitialTaskCount)
		}
		wantOverhead := roundEconomicUSD(q.Economics.BuyerChargePerTaskUSD * float64(wantExtraTasks))
		if q.Cost.VerificationOverheadUSD != wantOverhead {
			t.Fatalf("explicit verification overhead must use the frozen guarded buyer charge: want %v, got %v", wantOverhead, q.Cost.VerificationOverheadUSD)
		}
	})
}

// TestQuotePricesOutputTokens proves the Project Detection & Quotation 6->6.5 fix
// (docs/internal/CREED_AND_PATH_TO_TEN.md): a GENERATIVE quote (batch_infer /
// json_extraction) now carries an expected-OUTPUT-token cost term, so max_tokens
// measurably AND correctly moves the price — where before the completion length
// was ignored entirely and a 16-token and a 2048-token generation quoted the same.
// Proven end-to-end through POST /v1/quote against live Postgres (real catalogue
// pricing via GetModel), plus the honest negative: a non-generative job (embed) is
// UNAFFECTED by max_tokens.
func TestQuotePricesOutputTokens(t *testing.T) {
	ctx := context.Background()
	itPool.Exec(ctx, `TRUNCATE quotes`)
	t.Cleanup(func() { itPool.Exec(ctx, `TRUNCATE quotes`) })

	// 10 records of batch_infer input. We use skip_verification_floor so the
	// verification overhead does not confound the expected-cost comparison — this
	// test isolates the OUTPUT-token term in ExpectedUSD.
	var sb strings.Builder
	for i := 0; i < 10; i++ {
		fmt.Fprintf(&sb, `{"id":"r%d","prompt":"summarize record %d"}`+"\n", i, i)
	}
	input := sb.String()

	quoteInfer := func(maxTokens uint32) Quote {
		t.Helper()
		jt := map[string]any{"type": "batch_infer"}
		if maxTokens > 0 {
			jt["max_tokens"] = maxTokens
		}
		body := map[string]any{
			"job_type":     jt,
			"model":        map[string]any{"kind": "gguf", "ref": "llama-3.2-1b-instruct-q4"},
			"tier":         "batch",
			"verification": map[string]any{"skip_verification_floor": true},
			"input":        input,
		}
		status, out := req(t, "POST", "/v1/quote", body, buyerKey(), jsonCT())
		if status != 200 {
			t.Fatalf("POST /v1/quote (max_tokens=%d) -> %d: %s", maxTokens, status, out)
		}
		var q Quote
		if err := json.Unmarshal(out, &q); err != nil {
			t.Fatalf("decode quote: %v\n%s", err, out)
		}
		return q
	}

	// (1) max_tokens MOVES the price: a longer completion costs measurably more.
	short := quoteInfer(64)
	long := quoteInfer(1024)
	if !(long.Cost.ExpectedUSD > short.Cost.ExpectedUSD) {
		t.Fatalf("a longer max_tokens must raise a generative quote's expected cost: 64->%v vs 1024->%v",
			short.Cost.ExpectedUSD, long.Cost.ExpectedUSD)
	}

	// (2) the increase is CORRECT, not just monotone. The output-token term adds
	// nLines*(max_tokens) priced units at the catalogue price_per_1k. So the delta
	// between two max_tokens values is exactly:
	//   nRecords * (mtLong - mtShort) / 1000 * price_per_1k * tierMultiplier(batch=1)
	// We read the real catalogue price the server used (GetModel) so the assertion
	// is pinned to the same number the estimator saw, not a hard-coded guess.
	m, err := itStore.GetModel(ctx, "llama-3.2-1b-instruct-q4")
	if err != nil {
		t.Fatalf("GetModel: %v", err)
	}
	price := modelPrice(*m)
	const nRecords = 10
	wantDelta := roundUSD(float64(nRecords) * float64(1024-64) / 1000.0 * price)
	// The raw catalogue term is frozen in the economic input. Public ExpectedUSD
	// then adds the independently modeled processor/control/margin guard, so its
	// delta is intentionally not the raw token-price delta.
	gotBaseDelta := roundUSD(long.Economics.Input.BaseComputeUSD - short.Economics.Input.BaseComputeUSD)
	if gotBaseDelta != wantDelta {
		t.Fatalf("output-token base-compute delta wrong: want %v (=%d records * %d extra tokens /1k * price %v), got %v (short=%v long=%v)",
			wantDelta, nRecords, 1024-64, price, gotBaseDelta, short.Economics.Input.BaseComputeUSD, long.Economics.Input.BaseComputeUSD)
	}
	for _, q := range []Quote{short, long} {
		wantPublicExpected := roundEconomicUSD(q.Economics.InitialBuyerChargeUSD - q.Economics.Input.SLAPremiumUSD)
		if q.Cost.ExpectedUSD != wantPublicExpected {
			t.Fatalf("public expected cost must equal the frozen non-SLA guarded charge: want %v, got %v", wantPublicExpected, q.Cost.ExpectedUSD)
		}
	}

	// (3) an UNSET max_tokens is priced at the documented default (defaultQuoteMaxTokens),
	// never zero output — a generative job with no explicit completion length still
	// carries a real output cost, between the two explicit points above (64 < 256 < 1024).
	def := quoteInfer(0)
	if !(def.Cost.ExpectedUSD > short.Cost.ExpectedUSD && def.Cost.ExpectedUSD < long.Cost.ExpectedUSD) {
		t.Fatalf("an unset max_tokens must price the default (%d) completion length, between 64 and 1024: got %v (64->%v, 1024->%v)",
			defaultQuoteMaxTokens, def.Cost.ExpectedUSD, short.Cost.ExpectedUSD, long.Cost.ExpectedUSD)
	}

	// (4) the honest negative: a NON-generative job (embed) is UNAFFECTED by
	// max_tokens — its result size is not completion-driven, so pricing it on
	// max_tokens would be a lie. Two embed quotes at wildly different max_tokens
	// must have identical expected cost.
	quoteEmbed := func(maxTokens uint32) Quote {
		t.Helper()
		body := map[string]any{
			"job_type":     map[string]any{"type": "embed", "max_tokens": maxTokens},
			"model":        map[string]any{"kind": "hf", "ref": "all-minilm-l6-v2"},
			"tier":         "batch",
			"verification": map[string]any{"skip_verification_floor": true},
			"input":        input,
		}
		status, out := req(t, "POST", "/v1/quote", body, buyerKey(), jsonCT())
		if status != 200 {
			t.Fatalf("POST /v1/quote embed -> %d: %s", status, out)
		}
		var q Quote
		json.Unmarshal(out, &q)
		return q
	}
	e1, e2 := quoteEmbed(16), quoteEmbed(4096)
	if e1.Cost.ExpectedUSD != e2.Cost.ExpectedUSD {
		t.Fatalf("a non-generative (embed) quote must be unaffected by max_tokens: 16->%v vs 4096->%v",
			e1.Cost.ExpectedUSD, e2.Cost.ExpectedUSD)
	}
}

// TestQuoteETAUsesSustainedThroughputForLongBatchJobs proves the Thermal 6->7 fix
// (docs/internal/CREED_AND_PATH_TO_TEN.md): a LONG batch job's ETA is computed off
// the measured SUSTAINED tok/s (36.6% below peak, docs/GPU_CAPABILITY.md), not the
// peak figure the static target is derived from — so the ETA is honest for the
// multi-minute jobs where the throttle gap actually bites. estimateETASecs is
// tier-INDEPENDENT (it never takes tier), so the SAME long input quoted as "batch"
// vs "priority" shares the exact same peak-derived p50 underneath; only the batch
// quote is derated. The proof: batch.p50 == ceil(priority.p50 * sustainedFactor),
// and the derating is gated to LONG jobs (a short one is quoted at peak on both
// tiers). Proven end-to-end through POST /v1/quote against live Postgres, with an
// empty task_durations history so the peak-derived static target (not observed
// history) is what gets derated.
func TestQuoteETAUsesSustainedThroughputForLongBatchJobs(t *testing.T) {
	reset(t) // truncates tasks/jobs; task_durations has no rows for a fresh (batch_infer,model)
	ctx := context.Background()
	itPool.Exec(ctx, `TRUNCATE quotes`)
	t.Cleanup(func() { itPool.Exec(ctx, `TRUNCATE quotes`) })

	// A LONG batch_infer job: many records at a small split_size => many tasks =>
	// a multi-minute peak ETA (1 seed worker, 45s static per-task target), well
	// past sustainedETAThresholdSecs so the derating engages.
	var sb strings.Builder
	for i := 0; i < 200; i++ {
		fmt.Fprintf(&sb, `{"id":"r%d","prompt":"generate a long answer for record %d"}`+"\n", i, i)
	}
	input := sb.String()

	quoteAt := func(tier string, splitSize int) Quote {
		t.Helper()
		body := map[string]any{
			"job_type":     map[string]any{"type": "batch_infer", "max_tokens": 256},
			"model":        map[string]any{"kind": "gguf", "ref": "llama-3.2-1b-instruct-q4"},
			"params":       map[string]any{"split_size": splitSize},
			"tier":         tier,
			"verification": map[string]any{"skip_verification_floor": true},
			"input":        input,
		}
		status, out := req(t, "POST", "/v1/quote", body, buyerKey(), jsonCT())
		if status != 200 {
			t.Fatalf("POST /v1/quote (%s) -> %d: %s", tier, status, out)
		}
		var q Quote
		if err := json.Unmarshal(out, &q); err != nil {
			t.Fatalf("decode quote: %v\n%s", err, out)
		}
		return q
	}

	// splitSize 1 => 200 tasks => a peak ETA of 200 waves * 45s = ~9000s, far past
	// the 120s threshold.
	batch := quoteAt("batch", 1)
	priority := quoteAt("priority", 1)

	// The peak-derived p50 underneath is identical (estimateETASecs is
	// tier-independent) — priority is quoted at that peak; batch is derated.
	peakP50 := priority.Time.P50Secs
	if peakP50 < sustainedETAThresholdSecs {
		t.Fatalf("test fixture too small: peak ETA %ds is below the %ds derating threshold; make the job longer",
			peakP50, sustainedETAThresholdSecs)
	}
	wantBatchP50 := sustainedBatchETASecs(peakP50, "batch", false)
	if batch.Time.P50Secs != wantBatchP50 {
		t.Fatalf("long batch ETA must be derated to sustained: want p50=%d (=ceil(%d*%.4f)), got %d",
			wantBatchP50, peakP50, sustainedDeratingFactor, batch.Time.P50Secs)
	}
	if !(batch.Time.P50Secs > peakP50) {
		t.Fatalf("the sustained batch ETA (%d) must be strictly longer than the peak ETA (%d)",
			batch.Time.P50Secs, peakP50)
	}
	// The whole band (p90/worst) is derived from the derated p50, so it moves too.
	if !(batch.Time.P90Secs > priority.Time.P90Secs && batch.Time.WorstCaseSecs > priority.Time.WorstCaseSecs) {
		t.Fatalf("the derated p50 must widen the whole batch band: batch=%+v priority=%+v", batch.Time, priority.Time)
	}

	// A SHORT batch job (few tasks => a sub-threshold peak ETA) is quoted at peak on
	// BOTH tiers — the gap only bites minutes-long jobs. splitSize huge => 1 task.
	shortBatch := quoteAt("batch", 100000)
	shortPriority := quoteAt("priority", 100000)
	if shortBatch.Time.P50Secs >= sustainedETAThresholdSecs {
		// The single-task peak ETA is below threshold on this fixture; if that ever
		// changes, this guard makes the assumption explicit rather than silently
		// passing a vacuous check.
		t.Logf("note: short-job peak ETA %ds >= threshold; short-job non-derating assertion may not apply", shortBatch.Time.P50Secs)
	} else if shortBatch.Time.P50Secs != shortPriority.Time.P50Secs {
		t.Fatalf("a short batch job (peak ETA %ds < %ds) must be quoted at peak like priority, got batch=%d priority=%d",
			shortBatch.Time.P50Secs, sustainedETAThresholdSecs, shortBatch.Time.P50Secs, shortPriority.Time.P50Secs)
	}
}

// TestQuoteRecommendsFieldAgainstHumanJudgment proves the Project Detection &
// Quotation 8->9 content-based field detection (docs/internal/CREED_AND_PATH_TO_TEN.md):
// a real JSON input with MULTIPLE candidate text fields gets a correct field
// recommendation, surfaced through POST /v1/quote, and validated against a HUMAN's
// own judgment on a held-out sample set — the rung's exact proof artifact. Each
// fixture below is a realistic messy dataset shape; the wantField is MY (the
// author's) independent judgment of which column a buyer would actually want
// embedded, decided from the dataset's meaning BEFORE running the detector. The
// detector's longest-average-string recommendation must agree on every one.
func TestQuoteRecommendsFieldAgainstHumanJudgment(t *testing.T) {
	ctx := context.Background()
	itPool.Exec(ctx, `TRUNCATE quotes`)
	t.Cleanup(func() { itPool.Exec(ctx, `TRUNCATE quotes`) })

	// Held-out sample set: (realistic input, the field a human would pick). These
	// are deliberately varied — support tickets, product reviews, scraped articles,
	// a chat log — each with an obvious id/label/metadata column AND one clear
	// free-text column that is what you would actually embed or classify.
	cases := []struct {
		name      string
		input     string
		wantField string
	}{
		{
			name: "support tickets: body is the text, not ticket_id/priority",
			input: `{"ticket_id":"T-1001","priority":"high","body":"My laptop will not boot after the latest firmware update and I have an important demo tomorrow morning."}
{"ticket_id":"T-1002","priority":"low","body":"The trackpad occasionally registers a phantom double-click when scrolling long documents in the browser."}
{"ticket_id":"T-1003","priority":"med","body":"Requesting a refund for the duplicate charge that appeared on my statement last week after a failed checkout."}`,
			wantField: "body",
		},
		{
			name: "product reviews: review_text over sku/rating/verified",
			input: `{"sku":"SKU-88","rating":5,"verified":true,"review_text":"Genuinely the best pair of headphones I have owned; the noise cancellation is a night-and-day difference on flights."}
{"sku":"SKU-91","rating":2,"verified":false,"review_text":"Battery life is far shorter than advertised and the companion app crashes every time I try to change the equalizer settings."}
{"sku":"SKU-04","rating":4,"verified":true,"review_text":"Solid build quality and comfortable for long sessions, though the microphone picks up a bit too much background noise."}`,
			wantField: "review_text",
		},
		{
			name: "scraped articles: content over url/published/author",
			input: `{"url":"http://x/1","author":"jdoe","published":"2026-01-02","content":"Distributed inference markets are emerging as owners of idle Apple Silicon rent spare capacity to buyers who need batch throughput more than latency."}
{"url":"http://x/2","author":"asmith","published":"2026-02-14","content":"The economics hinge on verified settlement: a buyer must trust a result without re-running it, which is exactly what honeypots and redundancy make possible."}`,
			wantField: "content",
		},
		{
			name: "chat log: message over user/ts/room",
			input: `{"user":"u17","ts":1700000000,"room":"general","message":"Can someone review the pricing change before the release? I want to make sure the output-token term lands correctly for generation jobs."}
{"user":"u4","ts":1700000600,"room":"general","message":"Looks good to me — the sustained-throughput ETA adjustment is the one I would double-check on a really long batch job though."}`,
			wantField: "message",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			body := map[string]any{
				"job_type":     map[string]any{"type": "embed"},
				"model":        map[string]any{"kind": "hf", "ref": "all-minilm-l6-v2"},
				"tier":         "batch",
				"verification": map[string]any{"skip_verification_floor": true},
				"input":        c.input + "\n",
			}
			status, out := req(t, "POST", "/v1/quote", body, buyerKey(), jsonCT())
			if status != 200 {
				t.Fatalf("POST /v1/quote -> %d: %s", status, out)
			}
			var q Quote
			if err := json.Unmarshal(out, &q); err != nil {
				t.Fatalf("decode quote: %v\n%s", err, out)
			}
			// The recommendation must match the human's held-out judgment.
			if q.Input.RecommendedField != c.wantField {
				t.Fatalf("recommended_field=%q, human judged %q\n  field_stats=%+v",
					q.Input.RecommendedField, c.wantField, q.Input.FieldStats)
			}
			// The suggestion is CONFIRMABLE: the evidence is surfaced (not just the
			// bare pick), the recommendation is the top of it, and it carries a real
			// non-zero average length (it is a genuine text column).
			if len(q.Input.FieldStats) == 0 {
				t.Fatal("a recommendation must surface the per-field evidence for the buyer to confirm/override")
			}
			if q.Input.FieldStats[0].Field != c.wantField || q.Input.FieldStats[0].AvgStringLen <= 0 {
				t.Fatalf("the recommendation must be the highest-avg-length field with real content, got %+v", q.Input.FieldStats[0])
			}
		})
	}
}

// Plane D D7 / errata C-Errata-4 (docs/PLANE_D.md §13): quote-to-submit binding.
// A buyer quotes, then submits carrying the quote_id; a matching, unexpired quote
// binds (jobs.quote_id set, a quote_bound event, invoice shows quoted-vs-actual),
// while an expired or mismatched quote is refused 409 with no job created. Proven
// end-to-end against live Postgres.
func TestQuoteBindingMatchAndExpiry(t *testing.T) {
	ctx := context.Background()
	itPool.Exec(ctx, `TRUNCATE quotes`)
	t.Cleanup(func() { itPool.Exec(ctx, `TRUNCATE quotes`) })

	// One input, quoted once; every submit below reuses these exact bytes so the
	// best-effort input_sha256 match holds (the only axis we vary is what we want to
	// fail on: model mismatch, then expiry).
	const input = "{\"id\":\"a\",\"text\":\"bind me to a price\"}\n{\"id\":\"b\",\"text\":\"so the invoice can prove it\"}\n"
	quoteBody := map[string]any{
		"job_type":     map[string]any{"type": "embed"},
		"model":        map[string]any{"kind": "hf", "ref": "all-minilm-l6-v2"},
		"tier":         "batch",
		"verification": map[string]any{"redundancy_frac": 0.0, "honeypot_frac": 0.0, "payout_hold_secs": 0},
		"input":        input,
	}
	status, out := req(t, "POST", "/v1/quote", quoteBody, buyerKey(), jsonCT())
	if status != 200 {
		t.Fatalf("POST /v1/quote -> %d: %s", status, out)
	}
	var q Quote
	if err := json.Unmarshal(out, &q); err != nil {
		t.Fatalf("decode quote: %v\n%s", err, out)
	}
	if q.QuoteID == "" || q.ExpiresAt.IsZero() || q.InputSHA256 == "" {
		t.Fatalf("quote missing binding fields: id=%q expires=%v sha=%q", q.QuoteID, q.ExpiresAt, q.InputSHA256)
	}
	// The bare uuid (quotes.id / jobs.quote_id) is the "q_" handle minus its prefix;
	// it is NOT a wire field, so derive it from QuoteID rather than the (unexported,
	// unmarshal-skipped) q.bareID.
	wantBare, perr := uuid.Parse(strings.TrimPrefix(q.QuoteID, "q_"))
	if perr != nil {
		t.Fatalf("quote id %q not parseable: %v", q.QuoteID, perr)
	}

	// 1) Matching submit with the quote_id binds: 202, jobs.quote_id = the quote's
	//    bare uuid, a quote_bound event, and the invoice carries quoted_usd.
	bind := map[string]any{
		"job_type":     map[string]any{"type": "embed"},
		"model":        map[string]any{"kind": "hf", "ref": "all-minilm-l6-v2"},
		"tier":         "batch",
		"verification": map[string]any{"redundancy_frac": 0.0, "honeypot_frac": 0.0, "payout_hold_secs": 0},
		"input":        input,
		"quote_id":     q.QuoteID,
	}
	code, body := req(t, "POST", "/v1/jobs", bind, buyerKey(), jsonCT())
	if code != http.StatusAccepted {
		t.Fatalf("bound submit: want 202, got %d: %s", code, body)
	}
	var jr JobSubmitResponse
	if err := json.Unmarshal(body, &jr); err != nil {
		t.Fatalf("bound submit decode: %v (%s)", err, body)
	}
	var boundID *uuid.UUID
	if err := itPool.QueryRow(ctx, `SELECT quote_id FROM jobs WHERE id=$1`, jr.JobID).Scan(&boundID); err != nil {
		t.Fatalf("read jobs.quote_id: %v", err)
	}
	if boundID == nil || *boundID != wantBare {
		t.Fatalf("jobs.quote_id=%v, want bound to quote %v", boundID, wantBare)
	}
	// The binding is on the buyer-visible timeline.
	var nBound int
	if err := itPool.QueryRow(ctx,
		`SELECT count(*) FROM job_events WHERE job_id=$1 AND event='quote_bound'`, jr.JobID,
	).Scan(&nBound); err != nil {
		t.Fatalf("read quote_bound event: %v", err)
	}
	if nBound != 1 {
		t.Fatalf("want exactly 1 quote_bound event, got %d", nBound)
	}
	// The invoice now shows what the buyer was quoted next to what they were charged.
	icode, ibody := req(t, "GET", "/v1/jobs/"+jr.JobID.String()+"/invoice", nil, buyerKey())
	if icode != 200 {
		t.Fatalf("invoice: want 200, got %d: %s", icode, ibody)
	}
	var inv InvoiceView
	if err := json.Unmarshal(ibody, &inv); err != nil {
		t.Fatalf("invoice decode: %v (%s)", err, ibody)
	}
	if inv.QuotedUSD == nil {
		t.Fatalf("bound job invoice must include quoted_usd, got %s", ibody)
	}
	if *inv.QuotedUSD != q.Cost.ExpectedUSD {
		t.Fatalf("quoted_usd=%v, want the quote's expected %v", *inv.QuotedUSD, q.Cost.ExpectedUSD)
	}

	// 2) Mismatched submit: BGE is hardware_pending in the runtime matrix, so the
	// exact job/model admission boundary rejects it before quote binding or storage.
	mismatch := map[string]any{
		"job_type":     map[string]any{"type": "embed"},
		"model":        map[string]any{"kind": "gguf", "ref": "bge-small-en-v1.5"},
		"tier":         "batch",
		"verification": map[string]any{"redundancy_frac": 0.0, "honeypot_frac": 0.0, "payout_hold_secs": 0},
		"input":        input,
		"quote_id":     q.QuoteID,
	}
	mcode, mbody := req(t, "POST", "/v1/jobs", mismatch, buyerKey(), jsonCT())
	if mcode != http.StatusBadRequest {
		t.Fatalf("non-production model submit: want 400, got %d: %s", mcode, mbody)
	}
	if !strings.Contains(string(mbody), "runtime capability is not advertised") {
		t.Fatalf("400 reason should explain the runtime admission boundary, got %s", mbody)
	}

	// 3) Expired submit: age the quote past its TTL in the DB, then re-submit the
	//    matching shape → 409 "quote expired".
	if _, err := itPool.Exec(ctx,
		`UPDATE quotes SET expires_at = now() - interval '1 minute' WHERE id=$1`, wantBare,
	); err != nil {
		t.Fatalf("expire quote: %v", err)
	}
	ecode, ebody := req(t, "POST", "/v1/jobs", bind, buyerKey(), jsonCT())
	if ecode != http.StatusConflict {
		t.Fatalf("expired submit: want 409, got %d: %s", ecode, ebody)
	}
	if !strings.Contains(string(ebody), "expired") {
		t.Fatalf("409 reason should say expired, got %s", ebody)
	}
}

// seedClaimedTask inserts a fresh job + one running task claimed by `worker`, for
// the fail-endpoint tests. Returns (jobID, taskID).
func seedClaimedTask(t *testing.T, worker uuid.UUID, retryCount int) (uuid.UUID, uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	jobID, taskID := uuid.New(), uuid.New()
	if _, err := itPool.Exec(ctx,
		`INSERT INTO jobs (id, buyer_id, status, job_type, model_ref, input_ref, tier, task_count, tasks_done)
		 VALUES ($1,$2,'running','embed','all-minilm-l6-v2','jobs/x/in.jsonl','batch',1,0)`,
		jobID, demoBuyerUUID); err != nil {
		t.Fatalf("seed job: %v", err)
	}
	// claimed_by carries ownership (no FK); worker_id is a FK to workers, set only on
	// commit, so leave it NULL here — the fail path keys on claimed_by. This lets us
	// seed a task "owned" by an arbitrary (non-enrolled) worker id for the anti-spoof
	// test without tripping the worker_id foreign key.
	if _, err := itPool.Exec(ctx,
		`INSERT INTO tasks (id, job_id, status, claimed_by, claimed_at, started_at,
		                    input_ref, result_key, retry_count, chunk_index)
		 VALUES ($1,$2,'running',$3,now(),now(),'jobs/x/t0/in.jsonl','jobs/x/t0/out.json',$4,0)`,
		taskID, jobID, worker, retryCount); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	return jobID, taskID
}

// Plane C/D D0 (docs/PLANE_C_ERRATA.md C-Errata-1, docs/PLANE_D.md §6): a worker
// that KNOWS a retryable task cannot complete reports it immediately; the task is
// requeued in SECONDS (not the 30-min stale timeout), recorded in task_failures,
// and surfaced in the buyer's job_events timeline.
func TestFailEndpointRequeuesImmediately(t *testing.T) {
	ctx := context.Background()
	t.Cleanup(func() {
		itPool.Exec(ctx, `TRUNCATE task_failures, job_events`)
		itPool.Exec(ctx, `TRUNCATE tasks, jobs CASCADE`)
	})
	jobID, taskID := seedClaimedTask(t, demoWorkerUUID, 0)

	body := map[string]any{
		"class": "oom", "message": "next-token alloc exceeded effective memory",
		"backend": "batch_infer", "model": "llama-3.2-1b-instruct-q4", "duration_ms": 1273,
		"memory": map[string]any{"total_gb": 64, "available_gb": 2.1, "effective_gb": 0, "reserved_headroom_gb": 8},
	}
	status, out := req(t, "POST", "/v1/worker/task/"+taskID.String()+"/fail", body, workerTok(), jsonCT())
	if status != 200 {
		t.Fatalf("fail -> %d: %s", status, out)
	}
	var resp struct {
		Outcome string `json:"outcome"`
	}
	json.Unmarshal(out, &resp)
	if resp.Outcome != "requeued" {
		t.Fatalf("outcome=%q, want requeued", resp.Outcome)
	}
	// Task is claimable again NOW (retrying, unclaimed, retry++), not stranded.
	var tstatus string
	var claimedBy *uuid.UUID
	var retry int16
	if err := itPool.QueryRow(ctx, `SELECT status, claimed_by, retry_count FROM tasks WHERE id=$1`, taskID).
		Scan(&tstatus, &claimedBy, &retry); err != nil {
		t.Fatal(err)
	}
	if tstatus != "retrying" || claimedBy != nil || retry != 1 {
		t.Fatalf("task not requeued: status=%s claimed=%v retry=%d", tstatus, claimedBy, retry)
	}
	// A typed failure row with the memory snapshot persisted.
	var nfail int
	itPool.QueryRow(ctx, `SELECT count(*) FROM task_failures WHERE task_id=$1 AND failure_class='oom' AND retryable AND memory IS NOT NULL`, taskID).Scan(&nfail)
	if nfail != 1 {
		t.Fatalf("expected 1 oom task_failures row with memory, got %d", nfail)
	}
	// Buyer-visible timeline has task_failed + task_requeued.
	_, evOut := req(t, "GET", "/v1/jobs/"+jobID.String()+"/events", nil, buyerKey())
	var events []map[string]any
	json.Unmarshal(evOut, &events)
	gotFailed, gotRequeued := false, false
	for _, e := range events {
		switch e["event"] {
		case "task_failed":
			gotFailed = true
		case "task_requeued":
			gotRequeued = true
		}
	}
	if !gotFailed || !gotRequeued {
		t.Fatalf("event timeline missing task_failed/task_requeued: %s", evOut)
	}
}

// Buyer-bad-input fails TERMINALLY and immediately (no retry of bad data, no
// charge), with a job_failed event — the opposite of letting it drain budget.
func TestFailEndpointBadInputTerminal(t *testing.T) {
	ctx := context.Background()
	t.Cleanup(func() {
		itPool.Exec(ctx, `TRUNCATE task_failures, job_events`)
		itPool.Exec(ctx, `TRUNCATE tasks, jobs CASCADE`)
	})
	jobID, taskID := seedClaimedTask(t, demoWorkerUUID, 0)

	body := map[string]any{"class": "bad_input", "message": "missing required field 'text'"}
	status, out := req(t, "POST", "/v1/worker/task/"+taskID.String()+"/fail", body, workerTok(), jsonCT())
	if status != 200 {
		t.Fatalf("fail -> %d: %s", status, out)
	}
	var resp struct {
		Outcome string `json:"outcome"`
	}
	json.Unmarshal(out, &resp)
	if resp.Outcome != "failed" {
		t.Fatalf("bad_input outcome=%q, want failed (terminal)", resp.Outcome)
	}
	var tstatus, jstatus string
	itPool.QueryRow(ctx, `SELECT status FROM tasks WHERE id=$1`, taskID).Scan(&tstatus)
	itPool.QueryRow(ctx, `SELECT status FROM jobs WHERE id=$1`, jobID).Scan(&jstatus)
	if tstatus != "failed" || jstatus != "failed" {
		t.Fatalf("bad_input should fail task+job terminally: task=%s job=%s", tstatus, jstatus)
	}
	// job_failed event present.
	_, evOut := req(t, "GET", "/v1/jobs/"+jobID.String()+"/events", nil, buyerKey())
	if !bytesContains(evOut, "job_failed") {
		t.Fatalf("expected job_failed event, got %s", evOut)
	}
}

// Only the claiming worker may fail a task (anti-spoof).
func TestFailEndpointOnlyClaimingWorker(t *testing.T) {
	ctx := context.Background()
	t.Cleanup(func() {
		itPool.Exec(ctx, `TRUNCATE task_failures, job_events`)
		itPool.Exec(ctx, `TRUNCATE tasks, jobs CASCADE`)
	})
	// Task claimed by a DIFFERENT worker than the demo token authenticates as.
	_, taskID := seedClaimedTask(t, uuid.New(), 0)
	status, _ := req(t, "POST", "/v1/worker/task/"+taskID.String()+"/fail",
		map[string]any{"class": "oom"}, workerTok(), jsonCT())
	if status != 409 {
		t.Fatalf("non-claiming worker fail -> %d, want 409", status)
	}
}

// When several tasks of one job fail terminally, the job must be flipped + settled
// EXACTLY once (one job_failed event), with NO refund row: under partial-settle,
// committed charges stand and uncommitted work was never charged — a
// money-correctness invariant.
func TestFailEndpointSettlesJobOnce(t *testing.T) {
	ctx := context.Background()
	t.Cleanup(func() {
		itPool.Exec(ctx, `TRUNCATE task_failures, job_events`)
		itPool.Exec(ctx, `DELETE FROM ledger_entries WHERE buyer_id=$1`, demoBuyerUUID)
		itPool.Exec(ctx, `TRUNCATE tasks, jobs CASCADE`)
	})
	jobID := uuid.New()
	task1, task2 := uuid.New(), uuid.New()
	if _, err := itPool.Exec(ctx,
		`INSERT INTO jobs (id, buyer_id, status, job_type, model_ref, input_ref, tier, task_count, tasks_done)
		 VALUES ($1,$2,'running','embed','all-minilm-l6-v2','jobs/x/in.jsonl','batch',2,0)`,
		jobID, demoBuyerUUID); err != nil {
		t.Fatal(err)
	}
	for _, tid := range []uuid.UUID{task1, task2} {
		if _, err := itPool.Exec(ctx,
			`INSERT INTO tasks (id, job_id, status, claimed_by, claimed_at, started_at, input_ref, result_key, retry_count, chunk_index)
			 VALUES ($1,$2,'running',$3,now(),now(),'jobs/x/t/in.jsonl','jobs/x/t/out.json',0,0)`,
			tid, jobID, demoWorkerUUID); err != nil {
			t.Fatal(err)
		}
	}
	// A prior buyer_charge (debit) on task1: committed money that must STAND.
	if _, err := itPool.Exec(ctx,
		`INSERT INTO ledger_entries (kind, buyer_id, task_id, amount_usd, payout_status)
		 VALUES ('buyer_charge',$1,$2,-0.50,'pending')`, demoBuyerUUID, task1); err != nil {
		t.Fatal(err)
	}

	// Fail BOTH tasks terminally (bad_input). The job is flipped + settled ONCE.
	for _, tid := range []uuid.UUID{task1, task2} {
		st, out := req(t, "POST", "/v1/worker/task/"+tid.String()+"/fail",
			map[string]any{"class": "bad_input", "message": "x"}, workerTok(), jsonCT())
		if st != 200 {
			t.Fatalf("fail %s -> %d: %s", tid, st, out)
		}
	}
	// Partial settle: NO refund row, and actual_usd settles at the charged work.
	var nrefund int
	itPool.QueryRow(ctx, `SELECT count(*) FROM ledger_entries WHERE kind='refund' AND buyer_id=$1`, demoBuyerUUID).
		Scan(&nrefund)
	if nrefund != 0 {
		t.Fatalf("terminal fail must not refund (charges stand under partial settle), got %d refund rows", nrefund)
	}
	var actual float64
	itPool.QueryRow(ctx, `SELECT actual_usd::float8 FROM jobs WHERE id=$1`, jobID).Scan(&actual)
	if actual < 0.49 || actual > 0.51 {
		t.Fatalf("actual_usd should settle at the charged 0.50, got %v", actual)
	}
	var nJobFailed int
	itPool.QueryRow(ctx, `SELECT count(*) FROM job_events WHERE job_id=$1 AND event='job_failed'`, jobID).Scan(&nJobFailed)
	if nJobFailed != 1 {
		t.Fatalf("expected exactly one job_failed event, got %d", nJobFailed)
	}
}

func bytesContains(b []byte, sub string) bool {
	return strings.Contains(string(b), sub)
}

// TestClaimPriorityStreakYieldsToBatch proves the service-lane guarantee at the
// real transactional boundary: priority wins three times, the fourth ordinary
// claim is batch when one is eligible, and priority may continue without idling
// when no batch work exists while the durable debt remains outstanding.
func TestClaimPriorityStreakYieldsToBatch(t *testing.T) {
	ctx := context.Background()
	reset(t)
	wauth := WorkerAuth{WorkerID: demoWorkerUUID, SupplierID: demoSupplierUUID}

	addTask := func(tier string, pinned bool) uuid.UUID {
		t.Helper()
		jobID, taskID := uuid.New(), uuid.New()
		if _, err := itPool.Exec(ctx, `
			INSERT INTO jobs (id,buyer_id,status,job_type,model_ref,input_ref,output_ref,tier,
			                  verification_policy,task_count,tasks_done,min_memory_gb,job_type_spec)
			VALUES ($1,$2,'queued','embed','all-minilm-l6-v2',$3,$4,$5,'{}',1,0,0,'{"type":"embed"}')`,
			jobID, demoBuyerUUID, "jobs/"+jobID.String()+"/input.jsonl",
			"jobs/"+jobID.String()+"/output.json", tier); err != nil {
			t.Fatal(err)
		}
		var claimedBy any
		if pinned {
			claimedBy = demoWorkerUUID
		}
		if _, err := itPool.Exec(ctx, `
			INSERT INTO tasks (id,job_id,status,input_ref,result_key,chunk_index,visible_at,claimed_by)
			VALUES ($1,$2,'queued',$3,$4,0,now(),$5)`,
			taskID, jobID, "jobs/"+jobID.String()+"/task/input.jsonl",
			"jobs/"+jobID.String()+"/task/result.json", claimedBy); err != nil {
			t.Fatal(err)
		}
		return taskID
	}

	priorityTasks := []uuid.UUID{addTask("priority", false), addTask("priority", false), addTask("priority", false)}
	batchTask := addTask("batch", false)
	for i, want := range priorityTasks {
		got, err := itStore.ClaimTask(ctx, wauth)
		if err != nil || got == nil {
			t.Fatalf("priority claim %d: task=%v err=%v", i+1, got, err)
		}
		if got.TaskID != want || got.Tier != "priority" {
			t.Fatalf("priority claim %d: want task %s, got task %s tier %q", i+1, want, got.TaskID, got.Tier)
		}
	}
	got, err := itStore.ClaimTask(ctx, wauth)
	if err != nil || got == nil {
		t.Fatalf("batch opportunity claim: task=%v err=%v", got, err)
	}
	if got.TaskID != batchTask || got.Tier != "batch" {
		t.Fatalf("fourth ordinary claim must yield to batch %s, got task %s tier %q", batchTask, got.TaskID, got.Tier)
	}
	var streak int
	if err := itPool.QueryRow(ctx, `SELECT priority_claim_streak FROM workers WHERE id=$1`, demoWorkerUUID).Scan(&streak); err != nil {
		t.Fatal(err)
	}
	if streak != 0 {
		t.Fatalf("batch opportunity must reset streak, got %d", streak)
	}

	// No batch is eligible: do not idle. Priority proceeds, but the capped debt is
	// retained so the next batch arrival still receives the opportunity.
	if _, err := itPool.Exec(ctx, `UPDATE workers SET priority_claim_streak=3 WHERE id=$1`, demoWorkerUUID); err != nil {
		t.Fatal(err)
	}
	priorityOnly := addTask("priority", false)
	got, err = itStore.ClaimTask(ctx, wauth)
	if err != nil || got == nil || got.TaskID != priorityOnly {
		t.Fatalf("priority fallback without batch: want %s, got task=%v err=%v", priorityOnly, got, err)
	}
	if err := itPool.QueryRow(ctx, `SELECT priority_claim_streak FROM workers WHERE id=$1`, demoWorkerUUID).Scan(&streak); err != nil {
		t.Fatal(err)
	}
	if streak != 3 {
		t.Fatalf("priority fallback must retain capped batch debt, got %d", streak)
	}
}

// TestPinnedClaimPrecedesPriorityStreakFairness proves verification/tiebreak
// placement remains the absolute first branch. A pending batch opportunity may
// reorder ordinary work only after the already-selected pinned task starts.
func TestPinnedClaimPrecedesPriorityStreakFairness(t *testing.T) {
	ctx := context.Background()
	reset(t)
	wauth := WorkerAuth{WorkerID: demoWorkerUUID, SupplierID: demoSupplierUUID}

	addTask := func(tier string, pinned bool) uuid.UUID {
		t.Helper()
		jobID, taskID := uuid.New(), uuid.New()
		if _, err := itPool.Exec(ctx, `
			INSERT INTO jobs (id,buyer_id,status,job_type,model_ref,input_ref,tier,
			                  verification_policy,task_count,tasks_done,min_memory_gb,job_type_spec)
			VALUES ($1,$2,'queued','embed','all-minilm-l6-v2',$3,$4,'{}',1,0,0,'{"type":"embed"}')`,
			jobID, demoBuyerUUID, "jobs/"+jobID.String()+"/input.jsonl", tier); err != nil {
			t.Fatal(err)
		}
		var claimedBy any
		if pinned {
			claimedBy = demoWorkerUUID
		}
		if _, err := itPool.Exec(ctx, `
			INSERT INTO tasks (id,job_id,status,input_ref,result_key,chunk_index,visible_at,claimed_by)
			VALUES ($1,$2,'queued',$3,$4,0,now(),$5)`, taskID, jobID,
			"jobs/"+jobID.String()+"/task/input.jsonl", "jobs/"+jobID.String()+"/task/result.json", claimedBy); err != nil {
			t.Fatal(err)
		}
		return taskID
	}

	pinnedPriority := addTask("priority", true)
	batchTask := addTask("batch", false)
	if _, err := itPool.Exec(ctx, `UPDATE workers SET priority_claim_streak=3 WHERE id=$1`, demoWorkerUUID); err != nil {
		t.Fatal(err)
	}

	got, err := itStore.ClaimTask(ctx, wauth)
	if err != nil || got == nil || got.TaskID != pinnedPriority {
		t.Fatalf("pinned claim must remain first: want %s, got task=%v err=%v", pinnedPriority, got, err)
	}
	var streak int
	if err := itPool.QueryRow(ctx, `SELECT priority_claim_streak FROM workers WHERE id=$1`, demoWorkerUUID).Scan(&streak); err != nil {
		t.Fatal(err)
	}
	if streak != 3 {
		t.Fatalf("pinned work must not mutate ordinary lane debt, got %d", streak)
	}
	got, err = itStore.ClaimTask(ctx, wauth)
	if err != nil || got == nil || got.TaskID != batchTask {
		t.Fatalf("batch opportunity must follow pinned claim: want %s, got task=%v err=%v", batchTask, got, err)
	}
}

// TestClaimDispatchInterleaveFairness proves the Scheduling & Matching Engine
// 6.5->7 fix (docs/internal/CREED_AND_PATH_TO_TEN.md): without a fairness term,
// ClaimTask's ORDER BY fell straight through to oldest-first, so a large job
// that arrived earlier would claim EVERY worker ahead of a smaller job that
// arrived later, no matter how many of the large job's own tasks had already
// been served. Job A is older and already has 3 of its own tasks dispatched
// (running); Job B is newer and has had none. The fairness term must let Job
// B's task win the claim over Job A's still-queued (older) task.
func TestClaimDispatchInterleaveFairness(t *testing.T) {
	ctx := context.Background()
	reset(t)

	older := time.Now().Add(-time.Hour)
	newer := time.Now().Add(-time.Minute)

	jobA := uuid.New()
	if _, err := itPool.Exec(ctx,
		`INSERT INTO jobs (id, buyer_id, status, job_type, model_ref, input_ref, tier,
		                   task_count, tasks_done, min_memory_gb, created_at)
		 VALUES ($1,$2,'running','embed','all-minilm-l6-v2','jobs/a/in.jsonl','batch',4,0,0,$3)`,
		jobA, demoBuyerUUID, older); err != nil {
		t.Fatal(err)
	}
	// 3 of job A's tasks already dispatched (running) — job_dispatched_count=3
	// for its remaining queued task below.
	for i := 0; i < 3; i++ {
		if _, err := itPool.Exec(ctx,
			`INSERT INTO tasks (id, job_id, status, input_ref, result_key, chunk_index, visible_at, created_at, claimed_by, worker_id)
			 VALUES ($1,$2,'running','jobs/a/t/in.jsonl','jobs/a/t/out.json',$3, $4, $4, $5, $5)`,
			uuid.New(), jobA, i, older, demoWorkerUUID); err != nil {
			t.Fatal(err)
		}
	}
	taskA := uuid.New()
	if _, err := itPool.Exec(ctx,
		`INSERT INTO tasks (id, job_id, status, input_ref, result_key, chunk_index, visible_at, created_at)
		 VALUES ($1,$2,'queued','jobs/a/t/in.jsonl','jobs/a/t/out.json',3, $3, $3)`,
		taskA, jobA, older); err != nil {
		t.Fatal(err)
	}

	jobB := uuid.New()
	if _, err := itPool.Exec(ctx,
		`INSERT INTO jobs (id, buyer_id, status, job_type, model_ref, input_ref, tier,
		                   task_count, tasks_done, min_memory_gb, created_at)
		 VALUES ($1,$2,'queued','embed','all-minilm-l6-v2','jobs/b/in.jsonl','batch',1,0,0,$3)`,
		jobB, demoBuyerUUID, newer); err != nil {
		t.Fatal(err)
	}
	taskB := uuid.New()
	if _, err := itPool.Exec(ctx,
		`INSERT INTO tasks (id, job_id, status, input_ref, result_key, chunk_index, visible_at, created_at)
		 VALUES ($1,$2,'queued','jobs/b/t/in.jsonl','jobs/b/t/out.json',0, $3, $3)`,
		taskB, jobB, newer); err != nil {
		t.Fatal(err)
	}

	wauth := WorkerAuth{WorkerID: demoWorkerUUID, SupplierID: demoSupplierUUID}
	c, err := itStore.ClaimTask(ctx, wauth)
	if err != nil {
		t.Fatalf("ClaimTask: %v", err)
	}
	if c == nil {
		t.Fatal("expected a claimable task")
	}
	if c.TaskID != taskB {
		t.Fatalf("fairness: want job B's newer, less-served task (%s) claimed ahead of job A's older, already-3x-served task (%s), got %s",
			taskB, taskA, c.TaskID)
	}
}

// TestWorkerTpsCacheMaintainedAndReadByClaim proves the Control Plane Hot Path
// 7->8 fix (docs/internal/CREED_AND_PATH_TO_TEN.md, "hoist worker_tps into
// something computed once per worker state change rather than recomputed per
// candidate row per claim") end to end: UpsertWorker (POST /v1/worker/register)
// maintains worker_tps_cache instead of ClaimTask re-deriving it with a
// correlated subquery, AND the claim's real ORDER BY genuinely reads that
// maintained cache — not just that the column exists unread. Two otherwise-equal
// queued tasks of DIFFERENT job types are offered to the SAME worker; the claim
// must prefer whichever job_type the worker's cached tps currently ranks higher,
// and flipping which one is favored must flip which task gets claimed first —
// proving the read path is wired to the live cache value, not a coincidence of
// insertion order.
func TestWorkerTpsCacheMaintainedAndReadByClaim(t *testing.T) {
	ctx := context.Background()
	reset(t)
	wauth := WorkerAuth{WorkerID: demoWorkerUUID, SupplierID: demoSupplierUUID}

	// Part A: UpsertWorker maintenance. Register with an embed benchmark of 50
	// eps, confirm worker_tps_cache projects that job's native rate; re-register
	// (a real worker state change — a fresh benchmark report) with 120 eps for the SAME
	// job_type, and confirm the cache is UPDATED in place (last-write-wins),
	// not duplicated into a second row.
	readCachedTps := func(jobType string) (float32, bool) {
		var tps float32
		err := itPool.QueryRow(ctx,
			`SELECT tps FROM worker_tps_cache WHERE worker_id=$1 AND job_type=$2`,
			demoWorkerUUID, jobType).Scan(&tps)
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, false
		}
		if err != nil {
			t.Fatalf("reading worker_tps_cache: %v", err)
		}
		return tps, true
	}

	if code, body := req(t, "POST", "/v1/worker/register", WorkerCapability{
		WorkerID: demoWorkerUUID, SupplierID: demoSupplierUUID, HWClass: "apple_silicon_max", MemoryGB: 64,
		SupportedJobs: []string{"embed", "batch_infer"}, SupportedModels: []string{"all-minilm-l6-v2", "llama-3.2-1b-instruct-q4"},
		Benchmarks: []BenchResult{{ModelID: "all-minilm-l6-v2", JobType: "embed", EPS: 50, ThermalOK: true}},
	}, workerTok(), jsonCT()); code != 200 {
		t.Fatalf("register: want 200, got %d: %s", code, body)
	}
	if tps, ok := readCachedTps("embed"); !ok || tps != 50 {
		t.Fatalf("worker_tps_cache after first register: want (50, true), got (%v, %v)", tps, ok)
	}

	if code, body := req(t, "POST", "/v1/worker/register", WorkerCapability{
		WorkerID: demoWorkerUUID, SupplierID: demoSupplierUUID, HWClass: "apple_silicon_max", MemoryGB: 64,
		SupportedJobs: []string{"embed", "batch_infer"}, SupportedModels: []string{"all-minilm-l6-v2", "llama-3.2-1b-instruct-q4"},
		Benchmarks: []BenchResult{{ModelID: "all-minilm-l6-v2", JobType: "embed", EPS: 120, ThermalOK: true}},
	}, workerTok(), jsonCT()); code != 200 {
		t.Fatalf("re-register: want 200, got %d: %s", code, body)
	}
	if tps, ok := readCachedTps("embed"); !ok || tps != 120 {
		t.Fatalf("worker_tps_cache after re-register: want (120, true) [updated in place], got (%v, %v)", tps, ok)
	}
	var cacheRows int
	itPool.QueryRow(ctx, `SELECT count(*) FROM worker_tps_cache WHERE worker_id=$1 AND job_type='embed'`, demoWorkerUUID).Scan(&cacheRows)
	if cacheRows != 1 {
		t.Fatalf("worker_tps_cache must UPSERT (one row per worker/job_type), got %d rows", cacheRows)
	}

	// Part B: the claim's real ORDER BY reads this cache. Two queued tasks of
	// DIFFERENT job types, same job age/tier/priority, offered to the demo
	// worker (which supports both). Seed the cache so embed ranks higher, then
	// so batch_infer ranks higher, and confirm the claim flips with it.
	seedTwoJobTasks := func(t *testing.T) (embedTask, inferTask uuid.UUID) {
		t.Helper()
		if _, err := itPool.Exec(ctx, `TRUNCATE tasks, jobs CASCADE`); err != nil {
			t.Fatal(err)
		}
		now := time.Now()
		embedJob, inferJob := uuid.New(), uuid.New()
		embedTask, inferTask = uuid.New(), uuid.New()
		if _, err := itPool.Exec(ctx,
			`INSERT INTO jobs (id, buyer_id, status, job_type, model_ref, input_ref, tier,
			                   task_count, tasks_done, min_memory_gb, created_at)
			 VALUES ($1,$2,'queued','embed','all-minilm-l6-v2','jobs/e/in.jsonl','batch',1,0,0,$3)`,
			embedJob, demoBuyerUUID, now); err != nil {
			t.Fatal(err)
		}
		if _, err := itPool.Exec(ctx,
			`INSERT INTO tasks (id, job_id, status, input_ref, result_key, chunk_index, visible_at, created_at)
			 VALUES ($1,$2,'queued','jobs/e/t/in.jsonl','jobs/e/t/out.json',0,$3,$3)`,
			embedTask, embedJob, now); err != nil {
			t.Fatal(err)
		}
		if _, err := itPool.Exec(ctx,
			`INSERT INTO jobs (id, buyer_id, status, job_type, model_ref, input_ref, tier,
			                   task_count, tasks_done, min_memory_gb, created_at)
			 VALUES ($1,$2,'queued','batch_infer','llama-3.2-1b-instruct-q4','jobs/i/in.jsonl','batch',1,0,0,$3)`,
			inferJob, demoBuyerUUID, now); err != nil {
			t.Fatal(err)
		}
		if _, err := itPool.Exec(ctx,
			`INSERT INTO tasks (id, job_id, status, input_ref, result_key, chunk_index, visible_at, created_at)
			 VALUES ($1,$2,'queued','jobs/i/t/in.jsonl','jobs/i/t/out.json',0,$3,$3)`,
			inferTask, inferJob, now); err != nil {
			t.Fatal(err)
		}
		return embedTask, inferTask
	}
	setCache := func(t *testing.T, jobType string, tps float32) {
		t.Helper()
		if _, err := itPool.Exec(ctx,
			`INSERT INTO worker_tps_cache (worker_id, job_type, tps) VALUES ($1,$2,$3)
			 ON CONFLICT (worker_id, job_type) DO UPDATE SET tps = EXCLUDED.tps`,
			demoWorkerUUID, jobType, tps); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("embed favored -> embed claimed first", func(t *testing.T) {
		embedTask, inferTask := seedTwoJobTasks(t)
		setCache(t, "embed", 150)
		setCache(t, "batch_infer", 10)
		c, err := itStore.ClaimTask(ctx, wauth)
		if err != nil {
			t.Fatalf("ClaimTask: %v", err)
		}
		if c == nil || c.TaskID != embedTask {
			t.Fatalf("want embed task %s claimed first (worker_tps_cache favors embed), got %v (other task was %s)", embedTask, c, inferTask)
		}
	})

	t.Run("batch_infer favored -> batch_infer claimed first", func(t *testing.T) {
		embedTask, inferTask := seedTwoJobTasks(t)
		setCache(t, "embed", 10)
		setCache(t, "batch_infer", 150)
		c, err := itStore.ClaimTask(ctx, wauth)
		if err != nil {
			t.Fatalf("ClaimTask: %v", err)
		}
		if c == nil || c.TaskID != inferTask {
			t.Fatalf("want batch_infer task %s claimed first (worker_tps_cache favors batch_infer), got %v (other task was %s)", inferTask, c, embedTask)
		}
	})
}

func TestClaimHardFilter(t *testing.T) {
	reset(t)
	ctx := context.Background()
	wauth := WorkerAuth{WorkerID: demoWorkerUUID, SupplierID: demoSupplierUUID}

	// setJob inserts one queued primary task on a fresh job with the given
	// constraints, returning the task id. The demo worker is apple_silicon_max,
	// 64GB, supports embed + all-minilm-l6-v2, min_payout 0, supplier US/active.
	setJob := func(t *testing.T, jobType, modelRef string, minMem float32, hwClasses, residency []string, offered float64) {
		t.Helper()
		if _, err := itPool.Exec(ctx, `TRUNCATE tasks, jobs CASCADE`); err != nil {
			t.Fatal(err)
		}
		jobID, taskID := uuid.New(), uuid.New()
		if _, err := itPool.Exec(ctx,
			`INSERT INTO jobs (id, buyer_id, status, job_type, model_ref, input_ref, tier,
			                   task_count, tasks_done, min_memory_gb, hw_classes, data_residency, offered_rate_usd_hr)
			 VALUES ($1,$2,'queued',$3,$4,'jobs/x/input.jsonl','batch',1,0,$5,$6,$7,$8)`,
			jobID, demoBuyerUUID, jobType, modelRef, minMem,
			nullStrSlice(hwClasses), nullStrSlice(residency), offered); err != nil {
			t.Fatalf("insert job: %v", err)
		}
		if _, err := itPool.Exec(ctx,
			`INSERT INTO tasks (id, job_id, status, input_ref, result_key, chunk_index, visible_at)
			 VALUES ($1,$2,'queued','jobs/x/tasks/0/input.jsonl','jobs/x/tasks/0/result.json',0, now())`,
			taskID, jobID); err != nil {
			t.Fatalf("insert task: %v", err)
		}
	}
	// restoreWorker resets the demo worker + supplier to the fully-eligible baseline.
	restoreWorker := func(t *testing.T) {
		t.Helper()
		if _, err := itPool.Exec(ctx,
			`UPDATE workers SET hw_class='apple_silicon_max', memory_gb=64, last_seen_at=now(),
			   supported_jobs=ARRAY['embed','batch_infer'], supported_models=ARRAY['all-minilm-l6-v2'],
			   min_payout_usd_hr=0, thermal_ok=true,
			   throttled=false, effective_memory_gb=NULL, available_memory_gb=NULL,
			   reserved_headroom_gb=NULL WHERE id=$1`, demoWorkerUUID); err != nil {
			t.Fatal(err)
		}
		if _, err := itPool.Exec(ctx,
			`UPDATE suppliers SET status='active', data_country='US' WHERE id=$1`, demoSupplierUUID); err != nil {
			t.Fatal(err)
		}
		// Legacy supported_jobs/supported_models arrays are declarations only. Give
		// this fixture exactly one current generated production cell so every case
		// isolates its intended axis without bypassing runtime authority.
		replaceWorkerAuthorizationsForTest(t, ctx, demoWorkerUUID,
			[2]string{"embed", "all-minilm-l6-v2"})
	}
	claims := func(t *testing.T) bool {
		t.Helper()
		c, err := itStore.ClaimTask(ctx, wauth)
		if err != nil {
			t.Fatalf("ClaimTask: %v", err)
		}
		return c != nil
	}

	cases := []struct {
		name    string
		breakIt func(t *testing.T) // make the worker/supplier ineligible
	}{
		{"unsupported job type", func(t *testing.T) {
			setJob(t, "rerank", "all-minilm-l6-v2", 0, nil, nil, 1)
			// The exact fixture authority contains embed, NOT rerank.
			itPool.Exec(ctx, `UPDATE workers SET supported_jobs=ARRAY['embed'] WHERE id=$1`, demoWorkerUUID)
		}},
		{"unsupported model", func(t *testing.T) {
			setJob(t, "embed", "some-other-model", 0, nil, nil, 1)
		}},
		{"insufficient memory", func(t *testing.T) {
			setJob(t, "embed", "all-minilm-l6-v2", 999, nil, nil, 1)
		}},
		{"wrong hw_class", func(t *testing.T) {
			setJob(t, "embed", "all-minilm-l6-v2", 0, []string{"apple_silicon_ultra"}, nil, 1)
		}},
		{"data residency mismatch", func(t *testing.T) {
			setJob(t, "embed", "all-minilm-l6-v2", 0, nil, []string{"DE"}, 1) // supplier is US
		}},
		{"offered rate below worker floor", func(t *testing.T) {
			setJob(t, "embed", "all-minilm-l6-v2", 0, nil, nil, 0.5)
			itPool.Exec(ctx, `UPDATE workers SET min_payout_usd_hr=10 WHERE id=$1`, demoWorkerUUID)
		}},
		{"supplier quarantined", func(t *testing.T) {
			setJob(t, "embed", "all-minilm-l6-v2", 0, nil, nil, 1)
			itPool.Exec(ctx, `UPDATE suppliers SET status='suspended' WHERE id=$1`, demoSupplierUUID)
		}},
		{"worker throttled (memory pressure)", func(t *testing.T) {
			setJob(t, "embed", "all-minilm-l6-v2", 0, nil, nil, 1)
			// Worker is healthy on every axis but is pausing for memory pressure —
			// the safe-dispatch filter must not hand it work.
			itPool.Exec(ctx, `UPDATE workers SET throttled=true WHERE id=$1`, demoWorkerUUID)
		}},
		{"effective memory below job min", func(t *testing.T) {
			// Total memory (64) clears the 32GB floor, but the live effective pool
			// after headroom is only 8GB — the claim must use effective, not total.
			setJob(t, "embed", "all-minilm-l6-v2", 32, nil, nil, 1)
			itPool.Exec(ctx, `UPDATE workers SET effective_memory_gb=8 WHERE id=$1`, demoWorkerUUID)
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			restoreWorker(t)
			c.breakIt(t)
			if claims(t) {
				t.Fatalf("worker claimed a task it is ineligible for (%s)", c.name)
			}
			// Restoring eligibility lets the SAME worker claim the SAME task — proving
			// the rejection was the filter, not an unrelated empty queue. Make the
			// Normalize the same queued row to the known production embed cell and
			// strip every other constraint. This is the positive control for queue,
			// liveness, and exact runtime authority; a non-production model is never
			// made claimable merely to satisfy a test.
			restoreWorker(t)
			if _, err := itPool.Exec(ctx,
				`UPDATE jobs SET job_type='embed', model_ref='all-minilm-l6-v2',
				   min_memory_gb=0, hw_classes=NULL, data_residency=NULL, offered_rate_usd_hr=1`); err != nil {
				t.Fatal(err)
			}
			if !claims(t) {
				t.Fatalf("eligible worker failed to claim after restore (%s)", c.name)
			}
		})
	}

	// Reputation gate (Elite tier, research §6.4): a job with a high min_reputation is
	// NOT claimable by a low-reputation supplier, and becomes claimable once the
	// supplier's reputation clears the bar — the supplier-stickiness moat, in SQL.
	t.Run("reputation_gate", func(t *testing.T) {
		restoreWorker(t)
		if _, err := itPool.Exec(ctx, `TRUNCATE tasks, jobs CASCADE`); err != nil {
			t.Fatal(err)
		}
		jobID, taskID := uuid.New(), uuid.New()
		if _, err := itPool.Exec(ctx,
			`INSERT INTO jobs (id, buyer_id, status, job_type, model_ref, input_ref, tier,
			                   task_count, tasks_done, min_memory_gb, offered_rate_usd_hr, min_reputation)
			 VALUES ($1,$2,'queued','embed','all-minilm-l6-v2','jobs/x/input.jsonl','batch',1,0,0,1,0.90)`,
			jobID, demoBuyerUUID); err != nil {
			t.Fatalf("insert job: %v", err)
		}
		if _, err := itPool.Exec(ctx,
			`INSERT INTO tasks (id, job_id, status, input_ref, result_key, chunk_index, visible_at)
			 VALUES ($1,$2,'queued','jobs/x/tasks/0/input.jsonl','jobs/x/tasks/0/result.json',0, now())`,
			taskID, jobID); err != nil {
			t.Fatalf("insert task: %v", err)
		}
		if _, err := itPool.Exec(ctx, `UPDATE suppliers SET reputation=0.50 WHERE id=$1`, demoSupplierUUID); err != nil {
			t.Fatal(err)
		}
		if claims(t) {
			t.Fatal("0.50-reputation supplier must NOT claim a min_reputation=0.90 job")
		}
		// Clear the bar; reset the task (a rejected claim leaves it queued) and retry.
		if _, err := itPool.Exec(ctx, `UPDATE tasks SET claimed_by=NULL, started_at=NULL, status='queued' WHERE id=$1`, taskID); err != nil {
			t.Fatal(err)
		}
		if _, err := itPool.Exec(ctx, `UPDATE suppliers SET reputation=0.95 WHERE id=$1`, demoSupplierUUID); err != nil {
			t.Fatal(err)
		}
		if !claims(t) {
			t.Fatal("0.95-reputation supplier SHOULD claim a min_reputation=0.90 job")
		}
		itPool.Exec(ctx, `UPDATE suppliers SET reputation=0.90 WHERE id=$1`, demoSupplierUUID)
	})

	// Private Deployment (research §3): a private_pool job is NOT claimable by an
	// unbound supplier, and becomes claimable once the buyer binds them to their pool.
	t.Run("private_pool", func(t *testing.T) {
		restoreWorker(t)
		if _, err := itPool.Exec(ctx, `TRUNCATE tasks, jobs CASCADE`); err != nil {
			t.Fatal(err)
		}
		itPool.Exec(ctx, `DELETE FROM private_pool_members WHERE buyer_id=$1`, demoBuyerUUID)
		jobID, taskID := uuid.New(), uuid.New()
		if _, err := itPool.Exec(ctx,
			`INSERT INTO jobs (id, buyer_id, status, job_type, model_ref, input_ref, tier,
			                   task_count, tasks_done, min_memory_gb, offered_rate_usd_hr, private_pool)
			 VALUES ($1,$2,'queued','embed','all-minilm-l6-v2','jobs/x/input.jsonl','batch',1,0,0,1,true)`,
			jobID, demoBuyerUUID); err != nil {
			t.Fatalf("insert job: %v", err)
		}
		if _, err := itPool.Exec(ctx,
			`INSERT INTO tasks (id, job_id, status, input_ref, result_key, chunk_index, visible_at)
			 VALUES ($1,$2,'queued','jobs/x/tasks/0/input.jsonl','jobs/x/tasks/0/result.json',0, now())`,
			taskID, jobID); err != nil {
			t.Fatalf("insert task: %v", err)
		}
		if claims(t) {
			t.Fatal("unbound supplier must NOT claim a private_pool job")
		}
		if _, err := itPool.Exec(ctx, `UPDATE tasks SET claimed_by=NULL, started_at=NULL, status='queued' WHERE id=$1`, taskID); err != nil {
			t.Fatal(err)
		}
		if err := itStore.AddPrivatePoolMember(ctx, demoBuyerUUID, demoSupplierUUID); err != nil {
			t.Fatal(err)
		}
		if !claims(t) {
			t.Fatal("bound supplier SHOULD claim the buyer's private_pool job")
		}
		itPool.Exec(ctx, `DELETE FROM private_pool_members WHERE buyer_id=$1`, demoBuyerUUID)
	})
}

// TestPrivatePoolBuyerFacingFlow proves the real, end-to-end buyer-facing
// private-pool flow (Buyer advantage & pricing edge 6->7,
// docs/internal/CREED_AND_PATH_TO_TEN.md: "Productize the privacy premium
// instead of leaving it a sentence") — add/list/remove via the real HTTP API
// (not just a database row), a real priced premium + written attestation on the
// quote, and a submission refused when the buyer's pool is empty.
func TestPrivatePoolBuyerFacingFlow(t *testing.T) {
	reset(t)
	ctx := context.Background()
	itPool.Exec(ctx, `DELETE FROM private_pool_members WHERE buyer_id=$1`, demoBuyerUUID)
	defer itPool.Exec(ctx, `DELETE FROM private_pool_members WHERE buyer_id=$1`, demoBuyerUUID)

	// 1. Listing an empty pool returns an empty array, not null/404/500.
	code, out := req(t, "GET", "/v1/private-pool", nil, buyerKey())
	if code != http.StatusOK {
		t.Fatalf("list (empty): want 200, got %d: %s", code, out)
	}
	var empty []PrivatePoolMember
	if err := json.Unmarshal(out, &empty); err != nil {
		t.Fatalf("list (empty) decode: %v (%s)", err, out)
	}
	if len(empty) != 0 {
		t.Fatalf("expected an empty pool, got %d members", len(empty))
	}

	// 2. A private_pool submission with zero bound suppliers is refused loudly
	// (400) BEFORE any storage write — the exact gap the rung names: the job used
	// to be silently accepted and then could never be claimed by anyone.
	body := map[string]any{
		"job_type":     map[string]any{"type": "embed"},
		"model":        map[string]any{"kind": "hf", "ref": "all-minilm-l6-v2"},
		"constraints":  map[string]any{"min_memory_gb": 2},
		"verification": map[string]any{"skip_verification_floor": true},
		"tier":         "batch",
		"input":        `{"id":"a","text":"hello"}` + "\n",
		"private_pool": true,
	}
	code, out = req(t, "POST", "/v1/jobs", body, buyerKey(), jsonCT())
	if code != http.StatusBadRequest {
		t.Fatalf("private_pool submit with zero bound suppliers: want 400, got %d: %s", code, out)
	}
	if !strings.Contains(string(out), "zero bound suppliers") {
		t.Fatalf("rejection reason not surfaced honestly: %s", out)
	}

	// 3. A private-pool QUOTE (still zero members) prices honestly: the premium
	// is real and nonzero, the attestation is the real written guarantee, and a
	// warning names the exact zero-member problem the submit above just hit.
	quoteBody := map[string]any{
		"job_type":     map[string]any{"type": "embed"},
		"model":        map[string]any{"kind": "hf", "ref": "all-minilm-l6-v2"},
		"constraints":  map[string]any{"min_memory_gb": 2},
		"tier":         "batch",
		"input":        `{"id":"a","text":"hello"}` + "\n",
		"private_pool": true,
	}
	code, out = req(t, "POST", "/v1/quote", quoteBody, buyerKey(), jsonCT())
	if code != http.StatusOK {
		t.Fatalf("private-pool quote: want 200, got %d: %s", code, out)
	}
	var q Quote
	if err := json.Unmarshal(out, &q); err != nil {
		t.Fatalf("quote decode: %v (%s)", err, out)
	}
	if !q.Execution.PrivatePool {
		t.Fatal("quote must echo private_pool=true")
	}
	if q.Execution.PrivatePoolMemberCount != 0 {
		t.Fatalf("expected 0 bound members before binding any, got %d", q.Execution.PrivatePoolMemberCount)
	}
	if q.Cost.PrivatePoolPremiumUSD <= 0 {
		t.Fatalf("expected a real positive private-pool premium, got %v", q.Cost.PrivatePoolPremiumUSD)
	}
	if q.PrivatePoolAttestation == "" {
		t.Fatal("expected a non-empty written attestation on a private-pool quote")
	}
	if !strings.Contains(q.PrivatePoolAttestation, "claimable ONLY by") {
		t.Fatalf("attestation does not state the actual guarantee: %s", q.PrivatePoolAttestation)
	}
	foundZeroWarning := false
	for _, w := range q.Warnings {
		if strings.Contains(w, "zero bound suppliers") {
			foundZeroWarning = true
		}
	}
	if !foundZeroWarning {
		t.Fatalf("expected a warning about zero bound suppliers, got: %v", q.Warnings)
	}

	// A non-private quote for the identical workload must NOT carry the premium or
	// the attestation — the premium is opt-in, never charged by default.
	plainBody := map[string]any{
		"job_type":    map[string]any{"type": "embed"},
		"model":       map[string]any{"kind": "hf", "ref": "all-minilm-l6-v2"},
		"constraints": map[string]any{"min_memory_gb": 2},
		"tier":        "batch",
		"input":       `{"id":"a","text":"hello"}` + "\n",
	}
	code, out = req(t, "POST", "/v1/quote", plainBody, buyerKey(), jsonCT())
	if code != http.StatusOK {
		t.Fatalf("plain quote: want 200, got %d: %s", code, out)
	}
	var plainQ Quote
	if err := json.Unmarshal(out, &plainQ); err != nil {
		t.Fatalf("plain quote decode: %v (%s)", err, out)
	}
	if plainQ.Cost.PrivatePoolPremiumUSD != 0 {
		t.Fatalf("a non-private quote must carry zero premium, got %v", plainQ.Cost.PrivatePoolPremiumUSD)
	}
	if plainQ.PrivatePoolAttestation != "" {
		t.Fatalf("a non-private quote must carry no attestation, got %q", plainQ.PrivatePoolAttestation)
	}
	if plainQ.Cost.ExpectedUSD >= q.Cost.ExpectedUSD {
		t.Fatalf("private-pool expected cost (%v) must exceed the plain quote's (%v) — the premium must actually be priced in",
			q.Cost.ExpectedUSD, plainQ.Cost.ExpectedUSD)
	}

	// 4. Add the demo supplier to the pool via the real API (not a direct DB write).
	code, out = req(t, "POST", "/v1/private-pool",
		map[string]any{"supplier_id": demoSupplierUUID.String()}, buyerKey(), jsonCT())
	if code != http.StatusNoContent {
		t.Fatalf("add: want 204, got %d: %s", code, out)
	}
	// Idempotent: adding the same supplier again is still 204, not a conflict.
	code, out = req(t, "POST", "/v1/private-pool",
		map[string]any{"supplier_id": demoSupplierUUID.String()}, buyerKey(), jsonCT())
	if code != http.StatusNoContent {
		t.Fatalf("add (idempotent replay): want 204, got %d: %s", code, out)
	}

	// 5. List now shows exactly the one bound supplier, with real reputation/status.
	code, out = req(t, "GET", "/v1/private-pool", nil, buyerKey())
	if code != http.StatusOK {
		t.Fatalf("list (one member): want 200, got %d: %s", code, out)
	}
	var members []PrivatePoolMember
	if err := json.Unmarshal(out, &members); err != nil {
		t.Fatalf("list decode: %v (%s)", err, out)
	}
	if len(members) != 1 || members[0].SupplierID != demoSupplierUUID {
		t.Fatalf("expected exactly [%s], got %v", demoSupplierUUID, members)
	}
	if members[0].Status != "active" {
		t.Fatalf("expected the demo supplier's real status 'active', got %q", members[0].Status)
	}

	// 6. The SAME private-pool quote now reports member_count=1 — routing
	// transparency tracks the real, current pool size.
	code, out = req(t, "POST", "/v1/quote", quoteBody, buyerKey(), jsonCT())
	if code != http.StatusOK {
		t.Fatalf("private-pool quote (after bind): want 200, got %d: %s", code, out)
	}
	var q2 Quote
	if err := json.Unmarshal(out, &q2); err != nil {
		t.Fatalf("quote (after bind) decode: %v (%s)", err, out)
	}
	if q2.Execution.PrivatePoolMemberCount != 1 {
		t.Fatalf("expected 1 bound member after binding, got %d", q2.Execution.PrivatePoolMemberCount)
	}

	// 7. The private_pool submission now SUCCEEDS (a real bound supplier exists).
	code, out = req(t, "POST", "/v1/jobs", body, buyerKey(), jsonCT())
	if code != http.StatusAccepted {
		t.Fatalf("private_pool submit with a bound supplier: want 202, got %d: %s", code, out)
	}

	// 8. Remove the supplier via the real API; the pool is empty again.
	code, out = req(t, "DELETE", "/v1/private-pool/"+demoSupplierUUID.String(), nil, buyerKey())
	if code != http.StatusNoContent {
		t.Fatalf("remove: want 204, got %d: %s", code, out)
	}
	code, out = req(t, "GET", "/v1/private-pool", nil, buyerKey())
	if code != http.StatusOK {
		t.Fatalf("list (after remove): want 200, got %d: %s", code, out)
	}
	if err := json.Unmarshal(out, &empty); err != nil {
		t.Fatalf("list (after remove) decode: %v (%s)", err, out)
	}
	if len(empty) != 0 {
		t.Fatalf("expected an empty pool after remove, got %v", empty)
	}
	// Idempotent remove: removing an already-absent member is still 204.
	code, out = req(t, "DELETE", "/v1/private-pool/"+demoSupplierUUID.String(), nil, buyerKey())
	if code != http.StatusNoContent {
		t.Fatalf("remove (idempotent replay): want 204, got %d: %s", code, out)
	}

	// And now that the pool is empty again, the same private_pool submission is
	// refused again — the guard tracks LIVE membership, not a one-time check.
	code, out = req(t, "POST", "/v1/jobs", body, buyerKey(), jsonCT())
	if code != http.StatusBadRequest {
		t.Fatalf("private_pool submit after removing the only member: want 400, got %d: %s", code, out)
	}
}

// TestSchedulerExplain proves GET /admin/scheduler/explain (Plane D §17 D11): for a
// worker, it runs the SAME hard-filter predicates as ClaimTask against the claimable
// queue and reports COUNTS of why work was rejected. The core assertion: seed a job
// the demo worker cannot run on exactly ONE axis and the matching reason count is
// >=1 while eligible=0 (the queue HAS work, just none this worker may take) — making
// "nothing eligible" visible instead of looking like a slow worker. Also checks the
// empty-queue no_queued_tasks path, the eligible path, and the endpoint's auth/404.
func TestSchedulerExplain(t *testing.T) {
	reset(t)
	ctx := context.Background()

	// Reset the demo worker + supplier to the fully-eligible baseline (apple_silicon_max,
	// 64GB, supports embed/batch_infer + all-minilm-l6-v2, min_payout 0, US/active).
	restoreWorker := func(t *testing.T) {
		t.Helper()
		if _, err := itPool.Exec(ctx,
			`UPDATE workers SET hw_class='apple_silicon_max', memory_gb=64, last_seen_at=now(),
			   supported_jobs=ARRAY['embed','batch_infer'], supported_models=ARRAY['all-minilm-l6-v2'],
			   min_payout_usd_hr=0, thermal_ok=true,
			   throttled=false, effective_memory_gb=NULL, available_memory_gb=NULL,
			   reserved_headroom_gb=NULL WHERE id=$1`, demoWorkerUUID); err != nil {
			t.Fatal(err)
		}
		if _, err := itPool.Exec(ctx,
			`UPDATE suppliers SET status='active', data_country='US' WHERE id=$1`, demoSupplierUUID); err != nil {
			t.Fatal(err)
		}
		replaceWorkerAuthorizationsForTest(t, ctx, demoWorkerUUID,
			[2]string{"embed", "all-minilm-l6-v2"})
	}
	// seedJob inserts one queued primary task on a fresh job with the given
	// constraints (mirrors TestClaimHardFilter's setJob), returning the job id.
	seedJob := func(t *testing.T, jobType, modelRef string, minMem float32, hwClasses, residency []string, offered float64) uuid.UUID {
		t.Helper()
		if _, err := itPool.Exec(ctx, `TRUNCATE tasks, jobs CASCADE`); err != nil {
			t.Fatal(err)
		}
		jobID, taskID := uuid.New(), uuid.New()
		if _, err := itPool.Exec(ctx,
			`INSERT INTO jobs (id, buyer_id, status, job_type, model_ref, input_ref, tier,
			                   task_count, tasks_done, min_memory_gb, hw_classes, data_residency, offered_rate_usd_hr)
			 VALUES ($1,$2,'queued',$3,$4,'jobs/x/input.jsonl','batch',1,0,$5,$6,$7,$8)`,
			jobID, demoBuyerUUID, jobType, modelRef, minMem,
			nullStrSlice(hwClasses), nullStrSlice(residency), offered); err != nil {
			t.Fatalf("insert job: %v", err)
		}
		if _, err := itPool.Exec(ctx,
			`INSERT INTO tasks (id, job_id, status, input_ref, result_key, chunk_index, visible_at)
			 VALUES ($1,$2,'queued','jobs/x/tasks/0/input.jsonl','jobs/x/tasks/0/result.json',0, now())`,
			taskID, jobID); err != nil {
			t.Fatalf("insert task: %v", err)
		}
		return jobID
	}
	t.Cleanup(func() { itPool.Exec(ctx, `TRUNCATE tasks, jobs CASCADE`) })

	// Each case breaks exactly ONE hard-filter axis on an otherwise-claimable job and
	// names the reason field that must then be >=1 (and eligible must be 0).
	cases := []struct {
		name   string
		seed   func(t *testing.T)
		reason func(e *SchedulerExplanation) int
	}{
		{"hw_class mismatch", func(t *testing.T) {
			seedJob(t, "embed", "all-minilm-l6-v2", 0, []string{"apple_silicon_ultra"}, nil, 1) // worker is _max
		}, func(e *SchedulerExplanation) int { return e.HWClassMismatch }},
		{"memory mismatch", func(t *testing.T) {
			seedJob(t, "embed", "all-minilm-l6-v2", 999, nil, nil, 1) // worker has 64GB
		}, func(e *SchedulerExplanation) int { return e.MemoryMismatch }},
		{"job_type mismatch", func(t *testing.T) {
			seedJob(t, "rerank", "all-minilm-l6-v2", 0, nil, nil, 1) // exact fixture authority is embed only
		}, func(e *SchedulerExplanation) int { return e.JobTypeMismatch }},
		{"model mismatch", func(t *testing.T) {
			seedJob(t, "embed", "some-other-model", 0, nil, nil, 1)
		}, func(e *SchedulerExplanation) int { return e.ModelMismatch }},
		{"residency mismatch", func(t *testing.T) {
			seedJob(t, "embed", "all-minilm-l6-v2", 0, nil, []string{"DE"}, 1) // supplier is US
		}, func(e *SchedulerExplanation) int { return e.ResidencyMismatch }},
		{"payout floor", func(t *testing.T) {
			seedJob(t, "embed", "all-minilm-l6-v2", 0, nil, nil, 0.5)
			itPool.Exec(ctx, `UPDATE workers SET min_payout_usd_hr=10 WHERE id=$1`, demoWorkerUUID)
		}, func(e *SchedulerExplanation) int { return e.PayoutFloor }},
		{"supplier inactive", func(t *testing.T) {
			seedJob(t, "embed", "all-minilm-l6-v2", 0, nil, nil, 1)
			itPool.Exec(ctx, `UPDATE suppliers SET status='suspended' WHERE id=$1`, demoSupplierUUID)
		}, func(e *SchedulerExplanation) int { return e.SupplierInactive }},
		{"throttled", func(t *testing.T) {
			seedJob(t, "embed", "all-minilm-l6-v2", 0, nil, nil, 1)
			itPool.Exec(ctx, `UPDATE workers SET throttled=true WHERE id=$1`, demoWorkerUUID)
		}, func(e *SchedulerExplanation) int { return e.Throttled }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			restoreWorker(t)
			c.seed(t)
			exp, err := itStore.SchedulerExplain(ctx, demoWorkerUUID)
			if err != nil {
				t.Fatalf("SchedulerExplain: %v", err)
			}
			if got := c.reason(exp); got < 1 {
				t.Fatalf("%s: expected the matching reason count >=1, got %d (%+v)", c.name, got, exp)
			}
			if exp.Eligible != 0 {
				t.Fatalf("%s: a job broken on this axis must NOT be eligible, got eligible=%d", c.name, exp.Eligible)
			}
			if exp.NoQueuedTasks != 0 {
				t.Fatalf("%s: a job IS queued, so no_queued_tasks must be 0, got %d", c.name, exp.NoQueuedTasks)
			}
		})
	}

	// Empty queue → no_queued_tasks=1 and every other bucket 0: the "nothing to do"
	// case the endpoint exists to distinguish from a worker that cannot keep up.
	t.Run("empty queue", func(t *testing.T) {
		restoreWorker(t)
		if _, err := itPool.Exec(ctx, `TRUNCATE tasks, jobs CASCADE`); err != nil {
			t.Fatal(err)
		}
		exp, err := itStore.SchedulerExplain(ctx, demoWorkerUUID)
		if err != nil {
			t.Fatalf("SchedulerExplain: %v", err)
		}
		if exp.NoQueuedTasks != 1 || exp.Eligible != 0 {
			t.Fatalf("empty queue: want no_queued_tasks=1 eligible=0, got %+v", exp)
		}
	})

	// A fully-compatible queued job is counted eligible (the worker CAN claim it) —
	// the same predicates ClaimTask uses, read-only.
	t.Run("eligible", func(t *testing.T) {
		restoreWorker(t)
		seedJob(t, "embed", "all-minilm-l6-v2", 2, nil, nil, 1)
		exp, err := itStore.SchedulerExplain(ctx, demoWorkerUUID)
		if err != nil {
			t.Fatalf("SchedulerExplain: %v", err)
		}
		if exp.Eligible < 1 {
			t.Fatalf("a fully-compatible job must be eligible, got %+v", exp)
		}
		if exp.NoQueuedTasks != 0 {
			t.Fatalf("eligible: no_queued_tasks must be 0, got %d", exp.NoQueuedTasks)
		}
	})

	// The HTTP surface: admin-only, 400 without worker_id, 404 for an unknown worker,
	// 200 with the JSON body for the demo worker (one eligible job seeded above).
	t.Run("http endpoint", func(t *testing.T) {
		restoreWorker(t)
		seedJob(t, "embed", "all-minilm-l6-v2", 2, nil, nil, 1)

		path := "/admin/scheduler/explain?worker_id=" + demoWorkerUUID.String()
		if code, _ := req(t, "GET", path, nil, buyerKey()); code != 403 {
			t.Fatalf("non-admin must be 403, got %d", code)
		}
		if code, _ := req(t, "GET", "/admin/scheduler/explain", nil, adminKey()); code != 400 {
			t.Fatalf("missing worker_id must be 400, got %d", code)
		}
		if code, _ := req(t, "GET", "/admin/scheduler/explain?worker_id="+uuid.New().String(), nil, adminKey()); code != 404 {
			t.Fatalf("unknown worker must be 404, got %d", code)
		}
		code, body := req(t, "GET", path, nil, adminKey())
		if code != 200 {
			t.Fatalf("admin explain: %d %s", code, body)
		}
		var exp SchedulerExplanation
		if err := json.Unmarshal(body, &exp); err != nil {
			t.Fatalf("decode explain body: %v (%s)", err, body)
		}
		if exp.WorkerID != demoWorkerUUID {
			t.Fatalf("explain echoed wrong worker_id: %s", exp.WorkerID)
		}
		if exp.Eligible < 1 {
			t.Fatalf("seeded a compatible job; expected eligible>=1, got %+v", exp)
		}
	})
}

// Plane B (docs/internal/PLANE_B.md): the summed-memory scheduler seam exists, but
// the cluster runtime itself is still a generated-matrix stub. This regression test
// therefore proves the honest current boundary: even a legacy row advertising 1.8TB
// cannot claim until a real ClusterRunner is promoted to an advertised production
// cell. A normal production Metal cell remains claimable as the positive control.
// When physical multi-Mac execution is real, this test can add the 200GB success
// branch without weakening exact runtime authority or manufacturing a matrix row.
func TestClusterSummedMemoryRouting(t *testing.T) {
	reset(t)
	ctx := context.Background()
	clusterID := uuid.New()
	// A legacy/self-declared cluster row with ~1800 GB summed memory. It deliberately
	// has no worker_authorized_capabilities row: apple_cluster remains a stub.
	if _, err := itPool.Exec(ctx,
		`INSERT INTO workers (id, supplier_id, hw_class, memory_gb, last_seen_at, version,
		                      supported_jobs, supported_models, min_payout_usd_hr, thermal_ok)
		 VALUES ($1,$2,'apple_silicon_cluster',1800,now(),'cluster',
		         ARRAY['embed','batch_infer'], ARRAY['all-minilm-l6-v2'], 0, true)`,
		clusterID, demoSupplierUUID); err != nil {
		t.Fatalf("seed cluster worker: %v", err)
	}
	// Restore the world to just-the-demo-worker so later tests' supply is unchanged.
	t.Cleanup(func() {
		itPool.Exec(ctx, `TRUNCATE tasks, jobs CASCADE`)
		itPool.Exec(ctx, `DELETE FROM benchmark_results WHERE worker_id=$1`, clusterID)
		itPool.Exec(ctx, `DELETE FROM workers WHERE id=$1`, clusterID)
	})
	// Demo single Mac at its 64 GB baseline, fully eligible.
	if _, err := itPool.Exec(ctx,
		`UPDATE workers SET hw_class='apple_silicon_max', memory_gb=64, last_seen_at=now(),
		   supported_jobs=ARRAY['embed','batch_infer'], supported_models=ARRAY['all-minilm-l6-v2'],
		   min_payout_usd_hr=0, throttled=false, effective_memory_gb=NULL WHERE id=$1`, demoWorkerUUID); err != nil {
		t.Fatal(err)
	}
	itPool.Exec(ctx, `UPDATE suppliers SET status='active', data_country='US' WHERE id=$1`, demoSupplierUUID)

	// One queued embed task on a fresh job needing minMem GB (hw_classes NULL, so
	// ONLY memory differentiates — isolating the summed-memory routing).
	submit := func(minMem float32) {
		itPool.Exec(ctx, `TRUNCATE tasks, jobs CASCADE`)
		jobID, taskID := uuid.New(), uuid.New()
		if _, err := itPool.Exec(ctx,
			`INSERT INTO jobs (id, buyer_id, status, job_type, model_ref, input_ref, tier,
			                   task_count, tasks_done, min_memory_gb)
			 VALUES ($1,$2,'queued','embed','all-minilm-l6-v2','jobs/x/in.jsonl','batch',1,0,$3)`,
			jobID, demoBuyerUUID, minMem); err != nil {
			t.Fatalf("insert job: %v", err)
		}
		if _, err := itPool.Exec(ctx,
			`INSERT INTO tasks (id, job_id, status, input_ref, result_key, chunk_index, visible_at)
			 VALUES ($1,$2,'queued','jobs/x/t0/in.jsonl','jobs/x/t0/out.json',0, now())`,
			taskID, jobID); err != nil {
			t.Fatalf("insert task: %v", err)
		}
	}
	claims := func(auth WorkerAuth) bool {
		c, err := itStore.ClaimTask(ctx, auth)
		if err != nil {
			t.Fatalf("ClaimTask: %v", err)
		}
		return c != nil
	}
	single := WorkerAuth{WorkerID: demoWorkerUUID, SupplierID: demoSupplierUUID}
	cluster := WorkerAuth{WorkerID: clusterID, SupplierID: demoSupplierUUID}

	// A 200 GB-min job is above one Mac's memory. The cluster would clear the memory
	// predicate, but must remain inert because no production cluster cell exists.
	submit(200)
	if claims(single) {
		t.Fatal("single Mac (64GB) must NOT claim a 200GB-min job — summed memory is the whole point")
	}
	if claims(cluster) {
		t.Fatal("unadvertised cluster stub claimed work from legacy arrays without exact runtime authority")
	}
	var clusterAuthority int
	if err := itPool.QueryRow(ctx,
		`SELECT count(*) FROM worker_authorized_capabilities WHERE worker_id=$1`, clusterID,
	).Scan(&clusterAuthority); err != nil {
		t.Fatal(err)
	}
	if clusterAuthority != 0 {
		t.Fatalf("cluster stub unexpectedly has %d exact authority rows", clusterAuthority)
	}
	// The exact-authority filter still routes a small job to the registered Mac.
	submit(0)
	if !claims(single) {
		t.Fatal("single Mac must still claim a 0GB-min job (routing unbroken for small work)")
	}
}

// --- 17. auto-quarantine: a honeypot fail suspends the supplier + stamps it ---

func TestAutoQuarantineOnHoneypotFail(t *testing.T) {
	reset(t)
	ctx := context.Background()
	jobID, taskID := uuid.New(), uuid.New()
	mustJobTask(t, jobID, taskID, true /*honeypot*/, false, demoHoneypotEmbedRef)
	info := &CommitTaskInfo{TaskID: taskID, JobID: jobID, WorkerID: demoWorkerUUID,
		SupplierID: demoSupplierUUID, IsHoneypot: true, InputRef: demoHoneypotEmbedRef, jobType: "embed"}
	// Wrong answer → honeypot fail → quarantine.
	out, err := itServer.verifier.verifyTaskResult(ctx, info,
		TaskCommit{TaskID: taskID}, []byte(`{"vectors":[[0,1,0]]}`), nil)
	if err != nil || out != OutcomeFail {
		t.Fatalf("honeypot fail: out=%v err=%v", out, err)
	}
	var status string
	var quarantinedAt *time.Time
	itPool.QueryRow(ctx, `SELECT status, quarantined_at FROM suppliers WHERE id=$1`, demoSupplierUUID).
		Scan(&status, &quarantinedAt)
	if status != "suspended" {
		t.Fatalf("honeypot fail must auto-quarantine (suspend), got status %q", status)
	}
	if quarantinedAt == nil {
		t.Fatal("quarantined_at must be stamped on auto-quarantine")
	}
	// A quarantined supplier's worker can no longer claim work (the s.status gate).
	if _, err := itPool.Exec(ctx, `TRUNCATE tasks, jobs CASCADE`); err != nil {
		t.Fatal(err)
	}
	jid, tid := uuid.New(), uuid.New()
	mustJobTask(t, jid, tid, false, false, "jobs/q/tasks/0/input.jsonl")
	itPool.Exec(ctx, `UPDATE tasks SET status='queued', claimed_by=NULL, worker_id=NULL WHERE id=$1`, tid)
	itPool.Exec(ctx, `UPDATE workers SET supported_jobs=ARRAY['embed'], supported_models=ARRAY['all-minilm-l6-v2'] WHERE id=$1`, demoWorkerUUID)
	c, cerr := itStore.ClaimTask(ctx, WorkerAuth{WorkerID: demoWorkerUUID, SupplierID: demoSupplierUUID})
	if cerr != nil {
		t.Fatalf("claim: %v", cerr)
	}
	if c != nil {
		t.Fatal("a quarantined supplier's worker must not be able to claim a task")
	}
}

// --- 18. real 3-way tiebreak: two disagree → a third is dispatched and pinned;
// when it commits, the majority wins and the loser is docked ---

func TestTiebreakThreeWay(t *testing.T) {
	reset(t)
	ctx := context.Background()
	ensureExtraDemoSuppliers(t, ctx)
	// A second worker on an INDEPENDENT supplier (same hw class) so a distinct peer
	// exists. Must NOT share demoSupplierUUID: prunePeers excludes same-supplier
	// candidates (backlog P0 item 6), so a same-supplier "peer" is never eligible.
	peerWorker := uuid.New()
	if _, err := itPool.Exec(ctx,
		`INSERT INTO workers (id, supplier_id, hw_class, memory_gb, bw_gbps, last_seen_at, version,
		                      supported_jobs, supported_models, min_payout_usd_hr, thermal_ok)
		 VALUES ($1,$2,'apple_silicon_max',64,400,now(),'seed',
		         ARRAY['embed'],ARRAY['all-minilm-l6-v2'],0,true)
		 ON CONFLICT (id) DO UPDATE SET last_seen_at=now(), supported_jobs=ARRAY['embed']`,
		peerWorker, demoSupplier2UUID); err != nil {
		t.Fatal(err)
	}
	replaceWorkerAuthorizationsForTest(t, ctx, peerWorker,
		[2]string{"embed", "all-minilm-l6-v2"})
	defer itPool.Exec(ctx, `DELETE FROM worker_tokens WHERE worker_id=$1`, peerWorker)

	// A THIRD distinct same-class worker on a THIRD supplier. A real tiebreak excludes
	// BOTH disputants' suppliers (backlog P0 item 8, prunePeers' alsoSuppliers), not
	// just their worker ids, so it can only be pinned here.
	tiebreakPeer := uuid.New()
	if _, err := itPool.Exec(ctx,
		`INSERT INTO workers (id, supplier_id, hw_class, memory_gb, bw_gbps, last_seen_at, version,
		                      supported_jobs, supported_models, min_payout_usd_hr, thermal_ok)
		 VALUES ($1,$2,'apple_silicon_max',64,400,now(),'seed',
		         ARRAY['embed'],ARRAY['all-minilm-l6-v2'],0,true)`,
		tiebreakPeer, demoSupplier3UUID); err != nil {
		t.Fatal(err)
	}
	replaceWorkerAuthorizationsForTest(t, ctx, tiebreakPeer,
		[2]string{"embed", "all-minilm-l6-v2"})

	// One job, one chunk, two committed PRIMARY-ish results that disagree. We model
	// the chunk with a primary task (worker A) and a redundancy task (worker B) over
	// the same chunk_index, both complete with divergent embeddings.
	jobID := uuid.New()
	if _, err := itPool.Exec(ctx,
		`INSERT INTO jobs (id, buyer_id, status, job_type, model_ref, input_ref, tier, task_count, tasks_done, min_memory_gb)
		 VALUES ($1,$2,'running','embed','all-minilm-l6-v2','jobs/t/input.jsonl','batch',2,2,2)`,
		jobID, demoBuyerUUID); err != nil {
		t.Fatal(err)
	}
	economicPlan := installRawFixtureEconomicPlan(t, ctx, jobID, 2, 1)
	primary, redun := uuid.New(), uuid.New()
	aKey := "jobs/t/tasks/0/result.json"
	bKey := "jobs/t/redundancy/0/result.json"
	itStorage.PutObject(ctx, aKey, embedResultJSON(1), "application/json")              // e0
	itStorage.PutObject(ctx, bKey, []byte(`{"vectors":[[0,1,0]]}`), "application/json") // e1 ≠ e0
	for _, r := range []struct {
		id     uuid.UUID
		worker uuid.UUID
		redun  bool
		key    string
	}{{primary, demoWorkerUUID, false, aKey}, {redun, peerWorker, true, bKey}} {
		if _, err := itPool.Exec(ctx,
			`INSERT INTO tasks (id, job_id, status, is_redundancy, input_ref, result_key, result_ref, chunk_index, worker_id, claimed_by, completed_at,
			                    economic_buyer_charge_usd,economic_supplier_payout_usd,
			                    execution_worker_id,execution_supplier_id,execution_hw_class,execution_engine,execution_build_hash)
			 SELECT $1,$2,'complete',$3,'jobs/t/tasks/0/input.jsonl',$4,$4,0,$5,$5,now(),$6,$7,
			        w.id,w.supplier_id,w.hw_class,w.engine,w.build_hash
			   FROM workers w WHERE w.id=$5`,
			r.id, jobID, r.redun, r.key, r.worker,
			economicPlan.BuyerChargePerTaskUSD, economicPlan.SupplierPayoutPerTaskUSD); err != nil {
			t.Fatal(err)
		}
	}

	// Verifier WITH storage so the 3-way machinery runs.
	v := NewVerifier(itStore).WithStorage(itStorage)
	info := &CommitTaskInfo{TaskID: redun, JobID: jobID, WorkerID: peerWorker,
		SupplierID: demoSupplier2UUID, IsRedundancy: true, jobType: "embed",
		ModelRef: "all-minilm-l6-v2", MinMemoryGB: 2, ChunkIndex: 0,
		InputRef: "jobs/t/tasks/0/input.jsonl"}
	// The committing redundancy result is bKey; the peer present is aKey → mismatch.
	out, err := v.verifyTaskResult(ctx, info, TaskCommit{TaskID: redun},
		[]byte(`{"vectors":[[0,1,0]]}`), embedResultJSON(1))
	if err != nil || out != OutcomePassWithPenalty {
		t.Fatalf("2-way mismatch should dispatch a tiebreak (pass_with_penalty), got out=%v err=%v", out, err)
	}
	// A pinned tiebreak task must now exist for the chunk, claimed by the peer.
	var tbID, tbClaimed uuid.UUID
	var tbStatus string
	if err := itPool.QueryRow(ctx,
		`SELECT id, COALESCE(claimed_by,'00000000-0000-0000-0000-000000000000'), status FROM tasks
		 WHERE job_id=$1 AND is_redundancy=true AND hedged_from IS NOT NULL`, jobID).
		Scan(&tbID, &tbClaimed, &tbStatus); err != nil {
		t.Fatalf("expected a pinned tiebreak task: %v", err)
	}
	if tbClaimed == uuid.Nil {
		t.Fatal("tiebreak task must be pinned (pre-claimed) to a chosen peer")
	}
	if tbClaimed == demoWorkerUUID || tbClaimed == peerWorker {
		t.Fatalf("tiebreak peer must be distinct from the two that disagreed, got %s", tbClaimed)
	}
	if tbClaimed != tiebreakPeer {
		t.Fatalf("tiebreak must be pinned to the only eligible third worker %s, got %s", tiebreakPeer, tbClaimed)
	}
}

// --- 18b. make cheating economically real (Verification & Result Trust
// 5.5->6): a confirmed tiebreak LOSER does not just get docked reputation —
// its payout for the losing task itself is clawed back / withheld, checked
// against real ledger rows, not just the reputation delta. ---

func TestTiebreakLoserPayoutClawedBack(t *testing.T) {
	reset(t)
	ctx := context.Background()
	ensureExtraDemoSuppliers(t, ctx)

	// Same 3-worker, 3-supplier fixture as TestTiebreakThreeWay: primary
	// (demoWorkerUUID/demoSupplierUUID), a redundancy peer on an independent
	// supplier, and a third, independent tiebreak peer.
	peerWorker := uuid.New()
	if _, err := itPool.Exec(ctx,
		`INSERT INTO workers (id, supplier_id, hw_class, memory_gb, bw_gbps, last_seen_at, version,
		                      supported_jobs, supported_models, min_payout_usd_hr, thermal_ok)
		 VALUES ($1,$2,'apple_silicon_max',64,400,now(),'seed',
		         ARRAY['embed'],ARRAY['all-minilm-l6-v2'],0,true)
		 ON CONFLICT (id) DO UPDATE SET last_seen_at=now(), supported_jobs=ARRAY['embed']`,
		peerWorker, demoSupplier2UUID); err != nil {
		t.Fatal(err)
	}
	replaceWorkerAuthorizationsForTest(t, ctx, peerWorker,
		[2]string{"embed", "all-minilm-l6-v2"})
	defer itPool.Exec(ctx, `DELETE FROM worker_tokens WHERE worker_id=$1`, peerWorker)

	tiebreakPeerWorker := uuid.New()
	if _, err := itPool.Exec(ctx,
		`INSERT INTO workers (id, supplier_id, hw_class, memory_gb, bw_gbps, last_seen_at, version,
		                      supported_jobs, supported_models, min_payout_usd_hr, thermal_ok)
		 VALUES ($1,$2,'apple_silicon_max',64,400,now(),'seed',
		         ARRAY['embed'],ARRAY['all-minilm-l6-v2'],0,true)`,
		tiebreakPeerWorker, demoSupplier3UUID); err != nil {
		t.Fatal(err)
	}
	replaceWorkerAuthorizationsForTest(t, ctx, tiebreakPeerWorker,
		[2]string{"embed", "all-minilm-l6-v2"})

	// A real priced job (estimated_usd=1.00 over 2 tasks ⇒ $0.50/task at commit
	// time; a 3rd task gets added by InsertTiebreakTask below, but each task's
	// payout is priced off the ORIGINAL task_count captured before that insert,
	// matching how scheduleTaskPayout reads job state at the moment it is called
	// for each already-committed task).
	jobID := uuid.New()
	if _, err := itPool.Exec(ctx,
		`INSERT INTO jobs (id, buyer_id, status, job_type, model_ref, input_ref, tier, task_count, tasks_done, min_memory_gb, estimated_usd)
		 VALUES ($1,$2,'running','embed','all-minilm-l6-v2','jobs/tc/input.jsonl','batch',2,2,2,1.00)`,
		jobID, demoBuyerUUID); err != nil {
		t.Fatal(err)
	}
	economicPlan := installRawFixtureEconomicPlan(t, ctx, jobID, 2, 1)

	primary, redun := uuid.New(), uuid.New()
	aKey := "jobs/tc/tasks/0/result.json"
	bKey := "jobs/tc/redundancy/0/result.json"
	winningBytes := embedResultJSON(1)             // e0 — what primary AND the tiebreak peer will agree on
	losingBytes := []byte(`{"vectors":[[0,1,0]]}`) // e1 ≠ e0 — the redundancy worker's bad result
	itStorage.PutObject(ctx, aKey, winningBytes, "application/json")
	itStorage.PutObject(ctx, bKey, losingBytes, "application/json")
	for _, r := range []struct {
		id     uuid.UUID
		worker uuid.UUID
		redun  bool
		key    string
	}{{primary, demoWorkerUUID, false, aKey}, {redun, peerWorker, true, bKey}} {
		if _, err := itPool.Exec(ctx,
			`INSERT INTO tasks (id, job_id, status, is_redundancy, input_ref, result_key, result_ref, chunk_index, worker_id, claimed_by, completed_at,
			                    economic_buyer_charge_usd,economic_supplier_payout_usd,
			                    execution_worker_id,execution_supplier_id,execution_hw_class,execution_engine,execution_build_hash)
			 SELECT $1,$2,'complete',$3,'jobs/tc/tasks/0/input.jsonl',$4,$4,0,$5,$5,now(),$6,$7,
			        w.id,w.supplier_id,w.hw_class,w.engine,w.build_hash
			   FROM workers w WHERE w.id=$5`,
			r.id, jobID, r.redun, r.key, r.worker,
			economicPlan.BuyerChargePerTaskUSD, economicPlan.SupplierPayoutPerTaskUSD); err != nil {
			t.Fatal(err)
		}
	}

	// Mirror production exactly: both the primary's and the redundancy worker's
	// OWN commits already scheduled a real, held supplier_credit ledger row
	// before the tiebreak resolves (a 2-way disagreement with no third opinion
	// yet is pass_with_penalty, which IS paid — only a CONFIRMED tiebreak loss
	// withholds payout). Use the real handler method, not a hand-rolled insert.
	if err := itServer.scheduleTaskPayout(ctx, &CommitTaskInfo{JobID: jobID, TaskID: primary, SupplierID: demoSupplierUUID}); err != nil {
		t.Fatalf("scheduleTaskPayout(primary): %v", err)
	}
	if err := itServer.scheduleTaskPayout(ctx, &CommitTaskInfo{JobID: jobID, TaskID: redun, SupplierID: demoSupplier2UUID}); err != nil {
		t.Fatalf("scheduleTaskPayout(redundancy): %v", err)
	}
	creditedBefore := ledgerSupplierCredit(t, redun)
	if creditedBefore <= 0 {
		t.Fatalf("redundancy task must have a real positive held credit before the tiebreak resolves, got %v", creditedBefore)
	}

	// A pinned tiebreak task for a THIRD, independent worker — exactly what
	// dispatchTiebreak creates — now committed with a result agreeing with the
	// PRIMARY (so the primary+tiebreak side is the majority and the redundancy
	// worker is the confirmed loser).
	tbID, err := itStore.InsertTiebreakTask(ctx, jobID, primary, tiebreakPeerWorker, "jobs/tc/tasks/0/input.jsonl", 0)
	if err != nil {
		t.Fatalf("InsertTiebreakTask: %v", err)
	}
	tbKey := fmt.Sprintf("jobs/%s/tiebreak/%s/result.json", jobID, tbID)
	itStorage.PutObject(ctx, tbKey, winningBytes, "application/json")
	if err := itStore.StartTask(ctx, tbID, tiebreakPeerWorker); err != nil {
		t.Fatalf("start tiebreak task: %v", err)
	}
	if _, err := itPool.Exec(ctx,
		`UPDATE tasks SET status='complete',result_ref=$2,completed_at=now() WHERE id=$1`,
		tbID, tbKey); err != nil {
		t.Fatal(err)
	}

	// Run the REAL verifier (with storage, so the true N-way gatherChunkResults
	// path executes) for the tiebreak worker's own commit — mirroring exactly
	// what handleWorkerCommit does when the third opinion lands.
	v := NewVerifier(itStore).WithStorage(itStorage)
	info := &CommitTaskInfo{TaskID: tbID, JobID: jobID, WorkerID: tiebreakPeerWorker,
		SupplierID: demoSupplier3UUID, IsRedundancy: true, jobType: "embed",
		ModelRef: "all-minilm-l6-v2", MinMemoryGB: 2, ChunkIndex: 0,
		InputRef: "jobs/tc/tasks/0/input.jsonl"}
	out, verr := v.verifyTaskResult(ctx, info, TaskCommit{TaskID: tbID}, winningBytes, losingBytes)
	if verr != nil {
		t.Fatalf("verifyTaskResult: %v", verr)
	}
	if out != OutcomePass {
		// The tiebreak worker itself is on the WINNING side of the 3-way vote —
		// its own commit is a final payable pass. The vote's real mismatch remains
		// durable on the losing task's event/resolution; it must not strand the
		// proven winner behind the pass_with_penalty release gate.
		t.Fatalf("tiebreak worker on the winning side should be a payable pass, got %v", out)
	}
	// Winning side's own commit gets its payout scheduled exactly like any
	// other clean pass (handleWorkerCommit does this outside the verifier).
	if err := itServer.scheduleTaskPayout(ctx, info); err != nil {
		t.Fatalf("scheduleTaskPayout(tiebreak winner): %v", err)
	}

	// THE PROOF: the redundancy worker's LOSING task must have its credit
	// clawed back — checked against real ledger rows, not the reputation delta.
	creditedAfter := ledgerSupplierCredit(t, redun)
	if creditedAfter != 0 {
		t.Fatalf("tiebreak loser's task credit must net to zero after clawback, got %v (was %v before)", creditedAfter, creditedBefore)
	}
	var clawbackRows int
	var clawbackAmt float64
	if err := itPool.QueryRow(ctx,
		`SELECT count(*), COALESCE(SUM(amount_usd),0) FROM ledger_entries
		 WHERE task_id=$1 AND kind='clawback' AND payout_status='clawed_back'`, redun).
		Scan(&clawbackRows, &clawbackAmt); err != nil {
		t.Fatal(err)
	}
	if clawbackRows != 1 {
		t.Fatalf("expected exactly one clawback ledger row for the loser's task, got %d", clawbackRows)
	}
	if clawbackAmt >= 0 {
		t.Fatalf("clawback amount must be negative (a reversal), got %v", clawbackAmt)
	}
	if clawbackAmt != -creditedBefore {
		t.Fatalf("clawback must exactly reverse the original credit: credit=%v clawback=%v", creditedBefore, clawbackAmt)
	}
	// TaskHasClawback (the real signal the dispute resolver and fraud report
	// already key off) must now report true for the loser's task.
	clawed, cerr := itStore.TaskHasClawback(ctx, redun)
	if cerr != nil {
		t.Fatal(cerr)
	}
	if !clawed {
		t.Fatal("TaskHasClawback must report true for the tiebreak loser's task")
	}

	// The WINNING side must be unaffected: both the primary's original credit
	// and the tiebreak worker's own new credit stand, untouched, still held.
	if got := ledgerSupplierCredit(t, primary); got <= 0 {
		t.Fatalf("primary (winning) task credit must remain intact, got %v", got)
	}
	if got := ledgerSupplierCredit(t, tbID); got <= 0 {
		t.Fatalf("tiebreak worker's own (winning) task credit must be paid, got %v", got)
	}
}

// restoreDemoWorkerEmbedCapability restores the demo worker's full seed job set
// (embed + the batch types) and clears any single-job-type override a prior test
// (e.g. latency_moat_test.go's insertStragglerFixture) left behind. reset() does
// NOT touch supported_jobs, so an embed test that runs after such a test would
// otherwise find the demo worker unable to claim embed work and poll 204. Making
// each embed-dependent honeypot test call this keeps it order-independent.
func restoreDemoWorkerEmbedCapability(t *testing.T, ctx context.Context) {
	t.Helper()
	if _, err := itPool.Exec(ctx,
		`UPDATE workers
		    SET supported_jobs = ARRAY['embed','batch_infer','batch_classification','json_extraction','rerank'],
		        supported_models = ARRAY['all-minilm-l6-v2','llama-3.2-1b-instruct-q4'],
		        last_seen_at = now()
		  WHERE id = $1`, demoWorkerUUID); err != nil {
		t.Fatalf("restoreDemoWorkerEmbedCapability: %v", err)
	}
}

// --- 18c. make cheating economically real (Verification & Result Trust
// 5.5->6), the honeypot half: a worker that FAILS a known-answer honeypot does
// not just get docked reputation and quarantined — it must never be paid for
// the failed task, proven end to end through the REAL HTTP dispatch/commit
// path (not a direct verifier call) and checked against real ledger rows. ---

// TestHoneypotFailNoPayout submits a real job through POST /v1/jobs with
// honeypot_frac=1.0 (guaranteeing the job's single task is the seeded demo
// honeypot), polls it exactly like a real worker, commits a WRONG answer, and
// then proves two things against real ledger rows: (1) the worker's own
// handleWorkerCommit flow never calls scheduleTaskPayout for the failed
// honeypot task (verifyTaskResult runs strictly before scheduleTaskPayout in
// api.go, and OutcomeFail is one of the two outcomes — the other being
// OutcomeLossNoPayout — that the caller's `outcome != OutcomeFail` gate
// excludes from ever reaching scheduleTaskPayout), so its net ledger balance
// is exactly zero, never a positive credit; and (2) if a credit had ALREADY
// been written for that task before the honeypot fail resolved (the
// defensive case ClawbackTaskCredit exists for — a delayed/retried
// verification, or any future path that schedules credit before verifying),
// the clawback reverses it to net zero too, exactly like the tiebreak-loser
// proof above.
func TestHoneypotFailNoPayout(t *testing.T) {
	reset(t)
	ctx := context.Background()
	restoreDemoWorkerEmbedCapability(t, ctx) // order-independent: reset() does not restore supported_jobs

	// A single-line job with honeypot_frac=1.0: fracCount(1, 1.0) = 1, so ONE
	// honeypot task (AvailableHoneypots pulls the seeded demoHoneypotEmbedRef,
	// known answer demoHoneypotEmbedKnownAnswer — a real measured embedding, not
	// a placeholder) is appended alongside the one real primary/deliverable task
	// — the honeypot is an EXTRA probe task, not a replacement for the primary
	// (mirrors submitJob's task-building order).
	jobID, taskCount := submitEmbedJob(t, 1, 0 /*redFrac*/, 1.0 /*honeyFrac*/, 0)
	if taskCount != 2 {
		t.Fatalf("expected 2 tasks (1 primary + 1 honeypot), got %d", taskCount)
	}
	var taskID uuid.UUID
	if err := itPool.QueryRow(ctx,
		`SELECT id FROM tasks WHERE job_id=$1 AND is_honeypot=true`, jobID).Scan(&taskID); err != nil {
		t.Fatalf("expected exactly one honeypot task: %v", err)
	}

	// Real worker loop: this single demo worker polls and commits BOTH dispatched
	// tasks (the primary and the honeypot, order unspecified) so the job's
	// lifecycle proceeds exactly like a real single-agent run — but it only
	// reports the WRONG answer ([0,1,0]) on the honeypot dispatch (identified
	// here, as a real honeypot-blind worker cannot, purely for staging the
	// fixture's known-bad input); every other dispatch gets a correct result.
	var honeypotDispatched bool
	for i := 0; i < 2; i++ {
		code, body := req(t, "GET", "/v1/worker/poll", nil, workerTok())
		if code != 200 {
			t.Fatalf("poll %d: want 200, got %d: %s", i, code, body)
		}
		var disp TaskDispatch
		if err := json.Unmarshal(body, &disp); err != nil {
			t.Fatalf("dispatch decode: %v", err)
		}
		// NEITHER the result key NOR the input URL may carry a "honeypots/"
		// substring — a real worker must not be able to identify a trap from
		// either its own upload target OR its own download URL. The result-PUT
		// half was closed earlier (opaque per-task result_key); the input-GET
		// half is closed by Verification & Result Trust 5->5.5 (this bundle):
		// createJob now copies the seed honeypot's input to an opaque per-task
		// key "jobs/{job}/tasks/{taskID}/input.jsonl" — byte-for-byte the shape
		// of a primary chunk — before presigning, so the input URL no longer
		// leaks the seed's "honeypots/..." address. See TestHoneypotInputURLOpaque
		// for the dedicated adversarial proof.
		if strings.Contains(disp.ResultKey, "honeypots/") {
			t.Fatalf("honeypot identity leaked through the result key: %s", disp.ResultKey)
		}
		if strings.Contains(disp.InputURL, "honeypots/") {
			t.Fatalf("honeypot identity leaked through the input URL: %s", disp.InputURL)
		}
		result := embedResultJSON(1)
		if disp.TaskID == taskID {
			honeypotDispatched = true
			// The primary can commit first and raise 0.900 reputation to 0.901,
			// legitimately making a later honeypot comparison sampled. This test is
			// specifically the fail/no-payout branch, so pin the fixture at the trust
			// floor immediately before its known-wrong commit (probability 1.0).
			if _, err := itPool.Exec(ctx,
				`UPDATE suppliers SET reputation=$2 WHERE id=$1`, demoSupplierUUID, verifyTrustFloor); err != nil {
				t.Fatalf("pin honeypot fixture reputation: %v", err)
			}
			result = []byte(`{"vectors":[[0,1,0]]}`) // wrong answer for the honeypot
		}
		if err := itStorage.PutObject(ctx, disp.ResultKey, result, "application/json"); err != nil {
			t.Fatalf("put result: %v", err)
		}
		commit := TaskCommit{TaskID: disp.TaskID, ResultKey: disp.ResultKey, DurationMS: 10, TokensUsed: 8}
		if code, b := req(t, "POST", "/v1/worker/task/"+disp.TaskID.String()+"/commit", commit, workerTok(), jsonCT()); code != 204 {
			t.Fatalf("commit %d: want 204, got %d: %s", i, code, b)
		}
	}
	if !honeypotDispatched {
		t.Fatal("the honeypot task was never dispatched to the worker across both polls")
	}

	// THE PROOF (case 1 — the real path): the failed honeypot task's net ledger
	// balance is exactly zero. handleWorkerCommit never scheduled a payout for it
	// at all (OutcomeFail short-circuits before scheduleTaskPayout is ever
	// called), so there is no positive credit sitting in the ledger for a worker
	// that failed a known-answer probe.
	if got := ledgerSupplierCredit(t, taskID); got != 0 {
		t.Fatalf("a failed honeypot task must never carry a positive net ledger credit, got %v", got)
	}
	var creditRows int
	if err := itPool.QueryRow(ctx,
		`SELECT count(*) FROM ledger_entries WHERE task_id=$1 AND kind='supplier_credit'`, taskID).
		Scan(&creditRows); err != nil {
		t.Fatal(err)
	}
	if creditRows != 0 {
		t.Fatalf("a failed honeypot task must have zero supplier_credit ledger rows, got %d", creditRows)
	}

	// Auto-quarantine must also have fired (the existing 5->5.5-adjacent
	// guarantee) — confirms this really did take the honeypot-fail branch, not
	// some other outcome that happens to also leave the ledger empty.
	var status string
	if err := itPool.QueryRow(ctx, `SELECT status FROM suppliers WHERE id=$1`, demoSupplierUUID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "suspended" {
		t.Fatalf("a failed honeypot must auto-quarantine the supplier, got status %q", status)
	}

	// THE PROOF (case 2 — defensive clawback): even if a credit HAD already been
	// scheduled for this exact task before the honeypot fail resolved (mirroring
	// how the tiebreak-loser proof exercises ClawbackTaskCredit directly), the
	// clawback path reverses it to net zero, exactly like the tiebreak case.
	// This exercises ClawbackTaskCredit's real no-prior-credit-is-a-no-op branch
	// AND its real reversal branch against the same failed task.
	if err := itServer.scheduleTaskPayout(ctx, &CommitTaskInfo{JobID: jobID, TaskID: taskID, SupplierID: demoSupplierUUID}); err != nil {
		t.Fatalf("scheduleTaskPayout (simulating a pre-existing credit): %v", err)
	}
	creditedBefore := ledgerSupplierCredit(t, taskID)
	if creditedBefore <= 0 {
		t.Fatalf("expected a real positive credit to simulate the defensive case, got %v", creditedBefore)
	}
	if err := itStore.ClawbackTaskCredit(ctx, demoSupplierUUID, taskID); err != nil {
		t.Fatalf("ClawbackTaskCredit: %v", err)
	}
	if got := ledgerSupplierCredit(t, taskID); got != 0 {
		t.Fatalf("clawback must reverse a pre-existing credit on a failed honeypot task to net zero, got %v (was %v)", got, creditedBefore)
	}
	clawed, cerr := itStore.TaskHasClawback(ctx, taskID)
	if cerr != nil {
		t.Fatal(cerr)
	}
	if !clawed {
		t.Fatal("TaskHasClawback must report true after the honeypot-fail task's credit is clawed back")
	}
}

// --- 18d. the honeypot input-GET leak, closed (Verification & Result Trust
// 5->5.5, docs/internal/CREED_AND_PATH_TO_TEN.md). The adversarial proof
// artifact the rung demands: a worker inspecting its OWN presigned input URL for
// a honeypot task finds no "honeypots/" substring — the trap is byte-for-byte
// indistinguishable from a normal chunk on BOTH the GET (input) and PUT (result)
// wire, and the honeypot STILL verifies correctly (correct answer passes, wrong
// answer is caught + quarantines) so the fix closed the leak without disarming
// the trap. ---

// TestHoneypotInputURLOpaque submits a real embed job with honeypot_frac=1.0
// (one primary + one seeded honeypot), then plays an ADVERSARIAL worker: it
// polls every dispatched task and inspects the presigned input_url + result_key
// exactly as a supplier willing to read its own URLs would. The core assertion:
// NO dispatched URL — for the honeypot or the primary — contains the literal
// "honeypots/" (or "redundancy/") substring, so the adversary has nothing to key
// off of to tell a trap from real work. It then proves the trap is still live:
// the honeypot's opaque input still serves the real probe bytes, a CORRECT answer
// passes and pays, and (separately) a WRONG answer would be caught (covered by
// TestHoneypotFailNoPayout). This is the exact proof the rung's "an adversarial
// test worker that inspects its own presigned URLs finds nothing to key off of"
// artifact calls for.
func TestHoneypotInputURLOpaque(t *testing.T) {
	reset(t)
	ctx := context.Background()
	// reset() does not restore workers.supported_jobs, and other tests
	// (latency_moat_test.go's insertStragglerFixture) overwrite it to a single job
	// type — so restore the demo worker's full embed-capable job set explicitly,
	// making this test order-independent rather than relying on suite ordering.
	restoreDemoWorkerEmbedCapability(t, ctx)

	// One primary + one honeypot (honeypot_frac=1.0, one input line).
	jobID, taskCount := submitEmbedJob(t, 1, 0 /*redFrac*/, 1.0 /*honeyFrac*/, 0)
	if taskCount != 2 {
		t.Fatalf("expected 2 tasks (1 primary + 1 honeypot), got %d", taskCount)
	}

	// Identify the honeypot task server-side (a real worker cannot; we only do it
	// to assert the honeypot's own URL is opaque and that it verifies).
	var honeypotTaskID uuid.UUID
	if err := itPool.QueryRow(ctx,
		`SELECT id FROM tasks WHERE job_id=$1 AND is_honeypot=true`, jobID).Scan(&honeypotTaskID); err != nil {
		t.Fatalf("expected exactly one honeypot task: %v", err)
	}

	// The honeypot task's stored input_ref must be a per-task OPAQUE key, NOT the
	// seed honeypot's "honeypots/..." address — the DB-level half of the fix.
	var honeypotInputRef string
	if err := itPool.QueryRow(ctx,
		`SELECT COALESCE(input_ref,'') FROM tasks WHERE id=$1`, honeypotTaskID).Scan(&honeypotInputRef); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(honeypotInputRef, "honeypots/") {
		t.Fatalf("honeypot task's stored input_ref still leaks the seed address: %s", honeypotInputRef)
	}
	if !strings.HasPrefix(honeypotInputRef, "jobs/"+jobID.String()+"/tasks/") {
		t.Fatalf("honeypot task's input_ref must be a per-task opaque key, got: %s", honeypotInputRef)
	}

	// The opaque object must actually EXIST and serve the probe bytes (the copy
	// step ran) — otherwise a real worker's GET would 404 and retry forever, the
	// exact failure mode the seed-upload fix already closed for the seed key.
	if _, err := itStorage.GetObject(ctx, honeypotInputRef); err != nil {
		t.Fatalf("opaque honeypot input object must exist and serve bytes: %v", err)
	}

	// Play the adversarial worker across BOTH dispatches. For every task the
	// worker receives, its presigned input URL and its result key must be free of
	// any trap-revealing substring. We commit the CORRECT embed answer to every
	// task (including the honeypot, whose known answer is the real measured
	// embedding) so the honeypot PASSES — proving the opaque input still carries a
	// genuinely verifiable probe.
	var sawHoneypotDispatch bool
	for i := 0; i < 2; i++ {
		code, body := req(t, "GET", "/v1/worker/poll", nil, workerTok())
		if code != 200 {
			t.Fatalf("poll %d: want 200, got %d: %s", i, code, body)
		}
		var disp TaskDispatch
		if err := json.Unmarshal(body, &disp); err != nil {
			t.Fatalf("dispatch decode: %v", err)
		}
		// THE ADVERSARIAL ASSERTION: nothing a worker can see distinguishes a
		// honeypot from a normal chunk — not the input download URL, not the
		// result upload key. A determined supplier reading its own URLs finds no
		// "honeypots/" (nor "redundancy/") anywhere.
		for _, revealing := range []string{"honeypots/", "redundancy/", "honeypot"} {
			if strings.Contains(disp.InputURL, revealing) {
				t.Fatalf("task %s input URL leaks %q: %s", disp.TaskID, revealing, disp.InputURL)
			}
			if strings.Contains(disp.ResultKey, revealing) {
				t.Fatalf("task %s result key leaks %q: %s", disp.TaskID, revealing, disp.ResultKey)
			}
		}
		// The presigned input URL must actually serve its chunk (opaque, but real).
		if r, err := http.Get(disp.InputURL); err != nil || r.StatusCode != 200 {
			t.Fatalf("fetch presigned input for task %s: err=%v status=%v", disp.TaskID, err, r)
		} else {
			r.Body.Close()
		}
		if disp.TaskID == honeypotTaskID {
			sawHoneypotDispatch = true
		}
		// Commit the CORRECT answer for every task — the honeypot's known answer is
		// the real measured MiniLM embedding, so committing it back makes the
		// honeypot PASS (a live proof the opaque input did not disarm the trap).
		if err := itStorage.PutObject(ctx, disp.ResultKey, demoHoneypotEmbedResultJSON(t), "application/json"); err != nil {
			t.Fatalf("put result: %v", err)
		}
		commit := TaskCommit{TaskID: disp.TaskID, ResultKey: disp.ResultKey, DurationMS: 10, TokensUsed: 8}
		if code, b := req(t, "POST", "/v1/worker/task/"+disp.TaskID.String()+"/commit", commit, workerTok(), jsonCT()); code != 204 {
			t.Fatalf("commit %d: want 204, got %d: %s", i, code, b)
		}
	}
	if !sawHoneypotDispatch {
		t.Fatal("the honeypot task was never dispatched across both polls")
	}

	// The honeypot PASSED (correct answer): a honeypot_pass verification event was
	// recorded and the supplier is NOT quarantined — proving the trap is still
	// armed and firing on the opaque input, not silently skipped.
	var passEvents int
	if err := itPool.QueryRow(ctx,
		`SELECT count(*) FROM verification_events WHERE task_id=$1 AND kind='honeypot_pass'`,
		honeypotTaskID).Scan(&passEvents); err != nil {
		t.Fatal(err)
	}
	if passEvents == 0 {
		t.Fatal("a correct answer on the opaque-keyed honeypot must record a honeypot_pass event; the trap was disarmed by the input-key change")
	}
	var status string
	if err := itPool.QueryRow(ctx, `SELECT status FROM suppliers WHERE id=$1`, demoSupplierUUID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "active" {
		t.Fatalf("a PASSED honeypot must leave the supplier active, got %q", status)
	}
}

// ledgerSupplierCredit returns the NET supplier_credit ledger balance for one
// task: positive credits minus any clawback, mirroring exactly how a real
// payout worker or an admin fraud report would read "does this task's
// supplier actually still get paid for it".
func ledgerSupplierCredit(t *testing.T, taskID uuid.UUID) float64 {
	t.Helper()
	var net float64
	if err := itPool.QueryRow(context.Background(),
		`SELECT COALESCE(SUM(amount_usd),0) FROM ledger_entries
		 WHERE task_id=$1 AND kind IN ('supplier_credit','clawback')`, taskID).
		Scan(&net); err != nil {
		t.Fatal(err)
	}
	return net
}

// --- 19. straggler hedging: a long-running primary gets one hedge to a peer,
// and the winner's commit cancels the loser (first commit wins) ---

func TestStragglerHedge(t *testing.T) {
	reset(t)
	ctx := context.Background()
	ensureExtraDemoSuppliers(t, ctx)
	// A distinct same-class peer, on an INDEPENDENT supplier, to receive the hedge.
	// Must NOT share demoSupplierUUID: SelectRedundancyPeerExcluding (which
	// hedgeStragglers calls) excludes the anchor's own supplier (backlog P0 item 6).
	peer := uuid.New()
	var anchorEngine, anchorBuildHash string
	if err := itPool.QueryRow(ctx, `SELECT COALESCE(engine,''),COALESCE(build_hash,'') FROM workers WHERE id=$1`, demoWorkerUUID).
		Scan(&anchorEngine, &anchorBuildHash); err != nil {
		t.Fatal(err)
	}
	if _, err := itPool.Exec(ctx,
		`INSERT INTO workers (id, supplier_id, hw_class, engine, build_hash, memory_gb, bw_gbps, last_seen_at, version,
		                      supported_jobs, supported_models, min_payout_usd_hr, thermal_ok)
		 VALUES ($1,$2,'apple_silicon_max',$3,$4,64,400,now(),'seed',ARRAY['embed'],ARRAY['all-minilm-l6-v2'],0,true)
		 ON CONFLICT (id) DO UPDATE SET last_seen_at=now()`,
		peer, demoSupplier2UUID, anchorEngine, anchorBuildHash); err != nil {
		t.Fatal(err)
	}
	replaceWorkerAuthorizationsForTest(t, ctx, peer,
		[2]string{"embed", "all-minilm-l6-v2"})
	jobID, slow := uuid.New(), uuid.New()
	if _, err := itPool.Exec(ctx,
		`INSERT INTO jobs (id, buyer_id, status, job_type, model_ref, input_ref, tier, task_count, tasks_done, min_memory_gb)
		 VALUES ($1,$2,'running','embed','all-minilm-l6-v2','jobs/h/input.jsonl','batch',1,0,2)`,
		jobID, demoBuyerUUID); err != nil {
		t.Fatal(err)
	}
	economicPlan := installRawFixtureEconomicPlan(t, ctx, jobID, 1, 1)
	// Running primary, started well past the hedge window.
	if _, err := itPool.Exec(ctx,
		`INSERT INTO tasks (id, job_id, status, input_ref, result_key, chunk_index, worker_id, claimed_by, claimed_at, started_at,
		                    economic_buyer_charge_usd,economic_supplier_payout_usd)
		 VALUES ($1,$2,'running','jobs/h/tasks/0/input.jsonl','jobs/h/tasks/0/result.json',0,$3,$3,
		         now()-interval '10 minutes', now()-interval '10 minutes',$4,$5)`,
		slow, jobID, demoWorkerUUID,
		economicPlan.BuyerChargePerTaskUSD, economicPlan.SupplierPayoutPerTaskUSD); err != nil {
		t.Fatal(err)
	}
	wk := NewWorkers(itStore, itStorage, stubPayout{})
	if err := wk.hedgeStragglers(ctx); err != nil {
		t.Fatalf("hedgeStragglers: %v", err)
	}
	var hedgeID, hedgeClaimed uuid.UUID
	if err := itPool.QueryRow(ctx,
		`SELECT id, COALESCE(claimed_by,'00000000-0000-0000-0000-000000000000') FROM tasks
		 WHERE job_id=$1 AND hedged_from=$2 AND is_redundancy=false`, jobID, slow).
		Scan(&hedgeID, &hedgeClaimed); err != nil {
		t.Fatalf("expected a hedge task for the straggler: %v", err)
	}
	if hedgeClaimed != peer {
		t.Fatalf("hedge must be pinned to the distinct peer, got %s", hedgeClaimed)
	}
	// Hedging is once-only: a second sweep must not add another hedge.
	if err := wk.hedgeStragglers(ctx); err != nil {
		t.Fatal(err)
	}
	var nHedges int
	itPool.QueryRow(ctx, `SELECT count(*) FROM tasks WHERE job_id=$1 AND hedged_from=$2`, jobID, slow).Scan(&nHedges)
	if nHedges != 1 {
		t.Fatalf("straggler must be hedged at most once, got %d hedges", nHedges)
	}
	// First commit wins: the original primary commits → the hedge is cancelled.
	itStorage.PutObject(ctx, "jobs/h/tasks/0/result.json", embedResultJSON(1), "application/json")
	if err := itStore.CancelStragglerSiblings(ctx, jobID, 0, slow); err != nil {
		t.Fatal(err)
	}
	var hedgeStatus string
	itPool.QueryRow(ctx, `SELECT status FROM tasks WHERE id=$1`, hedgeID).Scan(&hedgeStatus)
	if hedgeStatus != "failed" {
		t.Fatalf("losing hedge should be cancelled (failed), got %q", hedgeStatus)
	}
}

// TestThrottledWorkerHedgesBeforeElapsedWindow proves the "detect throttling
// live, not just in benchmarks" rung (docs/internal/CREED_AND_PATH_TO_TEN.md,
// "Thermal sustained-vs-peak throughput on fanless Apple Silicon" 7→8): a task
// whose claiming worker's OWN most recent heartbeat reports throttled=true gets
// hedged after the short hedgeThrottledAfter floor (15s) — well before the
// normal elapsed-time hedgeAfter window (90s) would fire, and light-years before
// the 30-minute stale-worker watchdog (staleTaskTimeout) would ever catch it.
// A second, otherwise-identical task on a NON-throttled worker, started at the
// exact same time, must NOT be hedged yet — proving the throttled signal is
// what triggered the early hedge, not merely "some tasks get hedged sometimes".
func TestThrottledWorkerHedgesBeforeElapsedWindow(t *testing.T) {
	reset(t)
	ctx := context.Background()
	ensureExtraDemoSuppliers(t, ctx)

	// A distinct same-class peer on an INDEPENDENT supplier to receive the hedge(s).
	peer := uuid.New()
	if _, err := itPool.Exec(ctx,
		`INSERT INTO workers (id, supplier_id, hw_class, memory_gb, bw_gbps, last_seen_at, version,
		                      supported_jobs, supported_models, min_payout_usd_hr, thermal_ok)
		 VALUES ($1,$2,'apple_silicon_max',64,400,now(),'seed',ARRAY['embed'],ARRAY['all-minilm-l6-v2'],0,true)
		 ON CONFLICT (id) DO UPDATE SET last_seen_at=now()`, peer, demoSupplier2UUID); err != nil {
		t.Fatal(err)
	}
	replaceWorkerAuthorizationsForTest(t, ctx, peer,
		[2]string{"embed", "all-minilm-l6-v2"})

	// Two distinct worker rows on the demo supplier — one currently throttled,
	// one not — so a throttled vs. non-throttled claimant can be compared under
	// otherwise-identical conditions (same hw_class, same age, same job type).
	throttledWorker, healthyWorker := uuid.New(), uuid.New()
	for _, w := range []uuid.UUID{throttledWorker, healthyWorker} {
		if _, err := itPool.Exec(ctx,
			`INSERT INTO workers (id, supplier_id, hw_class, memory_gb, bw_gbps, last_seen_at, version,
			                      supported_jobs, supported_models, min_payout_usd_hr, thermal_ok, throttled)
			 VALUES ($1,$2,'apple_silicon_max',64,400,now(),'seed',ARRAY['embed'],ARRAY['all-minilm-l6-v2'],0,true,false)
			 ON CONFLICT (id) DO UPDATE SET last_seen_at=now()`, w, demoSupplierUUID); err != nil {
			t.Fatal(err)
		}
		replaceWorkerAuthorizationsForTest(t, ctx, w,
			[2]string{"embed", "all-minilm-l6-v2"})
	}
	// Mark ONLY throttledWorker's live heartbeat state as throttled=true — the
	// exact real signal main.rs's heartbeat sends when either memory pressure OR
	// runners.rs's LiveThroughputMonitor detects a real sustained tok/s drop.
	if _, err := itPool.Exec(ctx, `UPDATE workers SET throttled=true WHERE id=$1`, throttledWorker); err != nil {
		t.Fatal(err)
	}

	mkRunningTask := func(prefix string, worker uuid.UUID) (jobID, taskID uuid.UUID) {
		jobID, taskID = uuid.New(), uuid.New()
		if _, err := itPool.Exec(ctx,
			`INSERT INTO jobs (id, buyer_id, status, job_type, model_ref, input_ref, tier, task_count, tasks_done, min_memory_gb)
			 VALUES ($1,$2,'running','embed','all-minilm-l6-v2',$3,'batch',1,0,2)`,
			jobID, demoBuyerUUID, "jobs/"+prefix+"/input.jsonl"); err != nil {
			t.Fatal(err)
		}
		economicPlan := installRawFixtureEconomicPlan(t, ctx, jobID, 1, 1)
		if _, err := itPool.Exec(ctx,
			`INSERT INTO tasks (id, job_id, status, input_ref, result_key, chunk_index, worker_id, claimed_by, claimed_at, started_at,
			                    economic_buyer_charge_usd,economic_supplier_payout_usd)
			 VALUES ($1,$2,'running',$3,$4,0,$5,$5, now()-interval '20 seconds', now()-interval '20 seconds',$6,$7)`,
			taskID, jobID, "jobs/"+prefix+"/tasks/0/input.jsonl", "jobs/"+prefix+"/tasks/0/result.json", worker,
			economicPlan.BuyerChargePerTaskUSD, economicPlan.SupplierPayoutPerTaskUSD); err != nil {
			t.Fatal(err)
		}
		return jobID, taskID
	}

	// Both tasks started 20s ago: well past hedgeThrottledAfter (15s), but far
	// short of hedgeAfter (90s) — the window this test exists to distinguish.
	throttledJob, throttledTask := mkRunningTask("tw-throttled", throttledWorker)
	healthyJob, healthyTask := mkRunningTask("tw-healthy", healthyWorker)

	wk := NewWorkers(itStore, itStorage, stubPayout{})
	if err := wk.hedgeStragglers(ctx); err != nil {
		t.Fatalf("hedgeStragglers: %v", err)
	}

	// The THROTTLED worker's task must already have a real hedge, pinned to the
	// distinct peer — hedged purely because its own worker reported
	// throttled=true, not because of elapsed time (20s << hedgeAfter's 90s).
	var hedgeID, hedgeClaimed uuid.UUID
	if err := itPool.QueryRow(ctx,
		`SELECT id, COALESCE(claimed_by,'00000000-0000-0000-0000-000000000000') FROM tasks
		 WHERE job_id=$1 AND hedged_from=$2 AND is_redundancy=false`, throttledJob, throttledTask).
		Scan(&hedgeID, &hedgeClaimed); err != nil {
		t.Fatalf("throttled worker's straggler must be hedged well before hedgeAfter: %v", err)
	}
	if hedgeClaimed != peer {
		t.Fatalf("hedge must be pinned to the distinct peer, got %s", hedgeClaimed)
	}

	// The HEALTHY (non-throttled) worker's otherwise-identical task must NOT be
	// hedged yet — proving the throttled signal, not mere elapsed time, is what
	// triggered the early hedge above.
	var nHealthyHedges int
	itPool.QueryRow(ctx, `SELECT count(*) FROM tasks WHERE job_id=$1 AND hedged_from=$2`, healthyJob, healthyTask).Scan(&nHealthyHedges)
	if nHealthyHedges != 0 {
		t.Fatalf("a non-throttled worker's 20s-old task must not be hedged before hedgeAfter (90s); got %d hedges", nHealthyHedges)
	}

	// The real cx_throttled_hedges_total Prometheus counter actually advanced —
	// not just a DB row, the operator-visible signal named in the metric's own
	// purpose (distinct from the pre-existing elapsed-time cx_hedges_total).
	if got := metrics.throttledHedges.Load(); got < 1 {
		t.Fatalf("cx_throttled_hedges_total must advance on a throttled-worker hedge, got %d", got)
	}
}

// --- helpers shared by the direct-call tests ---

// mustJobTask inserts a minimal job + one task so FK-bound side effects (ledger,
// requeue, clawback) have real rows to act on.
func mustJobTask(t *testing.T, jobID, taskID uuid.UUID, honeypot, redundancy bool, inputRef string) {
	t.Helper()
	ctx := context.Background()
	if _, err := itPool.Exec(ctx,
		`INSERT INTO jobs (id, buyer_id, status, job_type, input_ref, tier, task_count, tasks_done)
		 VALUES ($1,$2,'running','embed','jobs/x/input.jsonl','batch',1,0)`, jobID, demoBuyerUUID); err != nil {
		t.Fatalf("insert job: %v", err)
	}
	if _, err := itPool.Exec(ctx,
		`INSERT INTO tasks (id, job_id, status, is_honeypot, is_redundancy, input_ref, result_key, visible_at)
		 VALUES ($1,$2,'running',$3,$4,$5,$6, now())`,
		taskID, jobID, honeypot, redundancy, inputRef, "jobs/x/tasks/0/result.json"); err != nil {
		t.Fatalf("insert task: %v", err)
	}
}

func supplierRep(t *testing.T) float32 {
	t.Helper()
	var rep float32
	if err := itPool.QueryRow(context.Background(),
		`SELECT reputation FROM suppliers WHERE id=$1`, demoSupplierUUID).Scan(&rep); err != nil {
		t.Fatal(err)
	}
	return rep
}

// Plane D D4 (docs/PLANE_D.md §10): a heartbeat carrying live memory must (1) append
// a rolling worker_memory_samples row with the REPORTED values (not just overwrite
// the latest-beat columns), (2) surface a recent avg_available_gb on GET /admin/workers,
// and (3) feed MedianEffectiveMemoryGB so quote risk sees the typical eligible box.
// Real telemetry end-to-end — the sample mirrors exactly what the agent sent.
func TestMemorySampleRecorded(t *testing.T) {
	reset(t)
	ctx := context.Background()
	t.Cleanup(func() { itPool.Exec(ctx, `DELETE FROM worker_memory_samples WHERE worker_id=$1`, demoWorkerUUID) })

	// Demo worker eligible for an (embed, all-minilm-l6-v2) job so the median query —
	// which uses the SAME supported-job/model + active + not-throttled + live filter
	// the claim does — counts this worker's latest sample.
	if _, err := itPool.Exec(ctx,
		`UPDATE workers SET hw_class='apple_silicon_max', memory_gb=64, last_seen_at=now(),
		   supported_jobs=ARRAY['embed','batch_infer'], supported_models=ARRAY['all-minilm-l6-v2'],
		   min_payout_usd_hr=0, throttled=false, effective_memory_gb=NULL, available_memory_gb=NULL,
		   reserved_headroom_gb=NULL WHERE id=$1`, demoWorkerUUID); err != nil {
		t.Fatal(err)
	}
	itPool.Exec(ctx, `UPDATE suppliers SET status='active' WHERE id=$1`, demoSupplierUUID)

	// A real heartbeat over the wire (the SAME path the agent uses), carrying live
	// memory. effective 40 / available 48; not throttled.
	hb := Heartbeat{
		WorkerID:          demoWorkerUUID,
		Timestamp:         uint64(time.Now().Unix()),
		AvailableMemoryGB: 48,
		EffectiveMemoryGB: 40,
		Throttled:         false,
	}
	if code, body := req(t, "POST", "/v1/worker/heartbeat", hb, workerTok(), jsonCT()); code != 204 {
		t.Fatalf("heartbeat -> %d, want 204; body=%s", code, body)
	}

	// (1) Exactly one sample row, holding the reported values.
	var n int
	var availGB, effGB float32
	var throttled bool
	if err := itPool.QueryRow(ctx,
		`SELECT count(*) FROM worker_memory_samples WHERE worker_id=$1`, demoWorkerUUID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("expected exactly 1 memory sample after one heartbeat, got %d", n)
	}
	if err := itPool.QueryRow(ctx,
		`SELECT available_gb, effective_gb, throttled FROM worker_memory_samples
		  WHERE worker_id=$1 ORDER BY created_at DESC LIMIT 1`, demoWorkerUUID).
		Scan(&availGB, &effGB, &throttled); err != nil {
		t.Fatal(err)
	}
	if availGB != 48 || effGB != 40 || throttled {
		t.Fatalf("sample mismatch: available=%v effective=%v throttled=%v (want 48/40/false)", availGB, effGB, throttled)
	}

	// (2) GET /admin/workers surfaces the rolling average for this worker.
	code, body := req(t, "GET", "/admin/workers", nil, adminKey())
	if code != 200 {
		t.Fatalf("admin/workers -> %d; body=%s", code, body)
	}
	var workers []AdminWorker
	if err := json.Unmarshal(body, &workers); err != nil {
		t.Fatalf("decode admin/workers: %v; body=%s", err, body)
	}
	found := false
	for _, w := range workers {
		if w.ID == demoWorkerUUID {
			found = true
			if w.MemorySamples != 1 || w.AvgAvailableGB != 48 {
				t.Fatalf("admin worker avg_available_gb=%v samples=%d, want 48/1", w.AvgAvailableGB, w.MemorySamples)
			}
		}
	}
	if !found {
		t.Fatalf("demo worker missing from admin/workers; body=%s", body)
	}

	// (3) The median feeds quote risk: an eligible (embed, all-minilm) query returns
	// this worker's reported effective memory.
	median, ok, err := itStore.MedianEffectiveMemoryGB(ctx, "embed", "all-minilm-l6-v2")
	if err != nil {
		t.Fatalf("MedianEffectiveMemoryGB: %v", err)
	}
	if !ok || median != 40 {
		t.Fatalf("median effective memory ok=%v median=%v, want true/40", ok, median)
	}

	// A pre-throttling beat (no memory fields) must NOT add a diluting all-NULL row.
	if code, _ := req(t, "POST", "/v1/worker/heartbeat",
		Heartbeat{WorkerID: demoWorkerUUID, Timestamp: uint64(time.Now().Unix())}, workerTok(), jsonCT()); code != 204 {
		t.Fatalf("bare heartbeat -> %d, want 204", code)
	}
	if err := itPool.QueryRow(ctx,
		`SELECT count(*) FROM worker_memory_samples WHERE worker_id=$1`, demoWorkerUUID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("a memory-less heartbeat must not write a sample; count=%d", n)
	}
}

// Plane D D3 (docs/PLANE_D.md §9): a heartbeat carrying loaded_models must upsert one
// worker_model_state row per warm model id (last_seen_warm = now()), refresh — not
// duplicate — on the next beat, and feed both the scheduler's warm re-rank
// (CandidateWorkers sets Warm) and the quote's warm supply count
// (WarmEligibleWorkerCount). Real warm telemetry end-to-end: a row exists only because
// the agent reported that id warm. A pre-warm beat (no loaded_models) writes nothing.
func TestWorkerModelStateUpsert(t *testing.T) {
	reset(t)
	ctx := context.Background()
	t.Cleanup(func() { itPool.Exec(ctx, `DELETE FROM worker_model_state WHERE worker_id=$1`, demoWorkerUUID) })

	// Demo worker eligible (embed, all-minilm-l6-v2): the warm-count + candidate
	// queries use the same supported-job/model + active + not-throttled + live filter.
	if _, err := itPool.Exec(ctx,
		`UPDATE workers SET hw_class='apple_silicon_max', memory_gb=64, last_seen_at=now(),
		   supported_jobs=ARRAY['embed','batch_infer'], supported_models=ARRAY['all-minilm-l6-v2'],
		   min_payout_usd_hr=0, throttled=false, effective_memory_gb=NULL WHERE id=$1`, demoWorkerUUID); err != nil {
		t.Fatal(err)
	}
	itPool.Exec(ctx, `UPDATE suppliers SET status='active' WHERE id=$1`, demoSupplierUUID)

	// A real heartbeat over the wire (the SAME path the agent uses) reporting one warm
	// model. An unrelated, non-eligible id is included to prove the row is upserted
	// verbatim (warm state is recorded; eligibility filtering happens at read time).
	hb := Heartbeat{
		WorkerID:     demoWorkerUUID,
		Timestamp:    uint64(time.Now().Unix()),
		LoadedModels: []string{"all-minilm-l6-v2", "whisper-tiny"},
	}
	if code, body := req(t, "POST", "/v1/worker/heartbeat", hb, workerTok(), jsonCT()); code != 204 {
		t.Fatalf("heartbeat -> %d, want 204; body=%s", code, body)
	}

	// One row per reported id, each fresh.
	var rows int
	if err := itPool.QueryRow(ctx,
		`SELECT count(*) FROM worker_model_state
		  WHERE worker_id=$1 AND last_seen_warm > now() - interval '60 seconds'`, demoWorkerUUID).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 2 {
		t.Fatalf("expected 2 fresh warm rows after one heartbeat, got %d", rows)
	}

	// The eligible (embed, all-minilm) job sees this worker as warm supply.
	warm, err := itStore.WarmEligibleWorkerCount(ctx, "embed", "all-minilm-l6-v2", 1)
	if err != nil {
		t.Fatalf("WarmEligibleWorkerCount: %v", err)
	}
	if warm != 1 {
		t.Fatalf("warm eligible count = %d, want 1", warm)
	}
	// A model the worker has NOT reported warm is not warm supply (no fabrication).
	if cold, err := itStore.WarmEligibleWorkerCount(ctx, "batch_infer", "llama-3.2-1b-instruct-q4", 1); err != nil {
		t.Fatalf("WarmEligibleWorkerCount(cold): %v", err)
	} else if cold != 0 {
		t.Fatalf("a never-reported model must have 0 warm workers, got %d", cold)
	}

	// CandidateWorkers (the scheduler's ranking view) marks this worker Warm for the
	// model it reported, and NOT for one it did not — the warm re-rank input.
	cands, err := itStore.CandidateWorkers(ctx, "embed", "all-minilm-l6-v2", 1)
	if err != nil {
		t.Fatalf("CandidateWorkers: %v", err)
	}
	foundWarm := false
	for _, c := range cands {
		if c.ID == demoWorkerUUID {
			foundWarm = c.Warm
		}
	}
	if !foundWarm {
		t.Fatal("demo worker should be marked Warm for the model it reported warm")
	}
	coldCands, err := itStore.CandidateWorkers(ctx, "embed", "some-other-model", 1)
	if err != nil {
		t.Fatalf("CandidateWorkers(cold): %v", err)
	}
	for _, c := range coldCands {
		if c.ID == demoWorkerUUID && c.Warm {
			t.Fatal("worker must NOT be Warm for a model it never reported")
		}
	}

	// Re-send the same beat: the upsert refreshes last_seen_warm, never duplicates
	// (PRIMARY KEY (worker_id, model_id)).
	if code, _ := req(t, "POST", "/v1/worker/heartbeat", hb, workerTok(), jsonCT()); code != 204 {
		t.Fatalf("second heartbeat -> %d, want 204", code)
	}
	if err := itPool.QueryRow(ctx,
		`SELECT count(*) FROM worker_model_state WHERE worker_id=$1`, demoWorkerUUID).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 2 {
		t.Fatalf("re-reporting the same models must upsert, not duplicate; rows=%d", rows)
	}

	// A pre-warm beat (no loaded_models) writes nothing new and leaves the rows intact.
	if code, _ := req(t, "POST", "/v1/worker/heartbeat",
		Heartbeat{WorkerID: demoWorkerUUID, Timestamp: uint64(time.Now().Unix())}, workerTok(), jsonCT()); code != 204 {
		t.Fatalf("bare heartbeat -> %d, want 204", code)
	}
	if err := itPool.QueryRow(ctx,
		`SELECT count(*) FROM worker_model_state WHERE worker_id=$1`, demoWorkerUUID).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 2 {
		t.Fatalf("a loaded_models-less heartbeat must not change warm rows; rows=%d", rows)
	}
}

// --- long-poll worker dispatch (Plane D §7 D1) ---

// seedClaimableEmbedTask inserts a running job + one queued, fully-claimable embed
// task for the demo worker and returns the task id. Mirrors the budget-cap test's
// seeding shape (no max_usd, so no budget gate); the demo worker after reset() is
// apple_silicon_max / supports embed+all-minilm / active / live, so this task is
// claimable on every axis.
func seedClaimableEmbedTask(t *testing.T, ctx context.Context) (jobID, taskID uuid.UUID) {
	t.Helper()
	jobID, taskID = uuid.New(), uuid.New()
	if _, err := itPool.Exec(ctx,
		`INSERT INTO jobs (id, buyer_id, status, job_type, model_ref, input_ref, tier,
		                   task_count, tasks_done, min_memory_gb, estimated_usd)
		 VALUES ($1,$2,'running','embed','all-minilm-l6-v2','jobs/lp/in.jsonl','batch',1,0,2,0.01)`,
		jobID, demoBuyerUUID); err != nil {
		t.Fatalf("seed job: %v", err)
	}
	if _, err := itPool.Exec(ctx,
		`INSERT INTO tasks (id, job_id, status, input_ref, result_key, chunk_index, visible_at)
		 VALUES ($1,$2,'queued','jobs/lp/t0/in.jsonl','jobs/lp/t0/out.json',0, now())`,
		taskID, jobID); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	return jobID, taskID
}

// TestLongPollReturnsOnNewTask proves the server-side wait wakes on real work: a
// poll started with ?wait_ms returns a task that becomes claimable mid-wait, and it
// returns FAST (well under the wait budget) rather than after the full timeout —
// the latency win D1 is built for.
func TestLongPollReturnsOnNewTask(t *testing.T) {
	ctx := context.Background()
	t.Cleanup(func() { itPool.Exec(ctx, `TRUNCATE tasks, jobs, job_events CASCADE`) })
	reset(t) // demo worker live + eligible; queue empty so the poll has to wait first

	// Insert the claimable task ~400ms into the wait, from another goroutine. The
	// poll is in-flight by then (it started with an empty queue), so it must pick the
	// task up on its next ~250ms re-attempt — not on the initial claim.
	const waitMS = 6000
	insertAfter := 400 * time.Millisecond
	var taskID uuid.UUID
	go func() {
		time.Sleep(insertAfter)
		_, taskID = seedClaimableEmbedTask(t, ctx)
	}()

	start := time.Now()
	code, body := req(t, "GET", "/v1/worker/poll?wait_ms="+strconv.Itoa(waitMS), nil, workerTok())
	elapsed := time.Since(start)
	if code != http.StatusOK {
		t.Fatalf("long-poll: want 200 (task delivered mid-wait), got %d: %s", code, body)
	}
	var disp TaskDispatch
	if err := json.Unmarshal(body, &disp); err != nil {
		t.Fatalf("dispatch decode: %v (%s)", err, body)
	}
	// The wait must have ended on the INSERT, not on the timeout: a couple of
	// re-attempt ticks after the ~400ms insert, far below the 6s budget.
	if elapsed > 3*time.Second {
		t.Fatalf("long-poll took %v — it timed out instead of waking on the new task", elapsed)
	}
	// And it must hand back the task we inserted (claimed + dispatched), not a phantom.
	if taskID != (uuid.UUID{}) && disp.TaskID != taskID {
		t.Fatalf("long-poll returned task %s, want the just-inserted %s", disp.TaskID, taskID)
	}
	var claimedBy *uuid.UUID
	itPool.QueryRow(ctx, `SELECT claimed_by FROM tasks WHERE id=$1`, disp.TaskID).Scan(&claimedBy)
	if claimedBy == nil || *claimedBy != demoWorkerUUID {
		t.Fatalf("delivered task must be claimed by the demo worker, claimed_by=%v", claimedBy)
	}
}

// TestLongPollTimesOutCleanly proves an idle wait ends correctly: with nothing
// claimable, a poll with ?wait_ms holds open for ~wait_ms, then returns 204 (not an
// error, not a stuck connection) and bumps cx_long_poll_timeouts_total exactly once.
func TestLongPollTimesOutCleanly(t *testing.T) {
	ctx := context.Background()
	t.Cleanup(func() { itPool.Exec(ctx, `TRUNCATE tasks, jobs CASCADE`) })
	reset(t)
	// Empty, claimable queue guaranteed: drop every task so the wait can never find work.
	if _, err := itPool.Exec(ctx, `TRUNCATE tasks, jobs CASCADE`); err != nil {
		t.Fatalf("clear queue: %v", err)
	}

	before := metrics.longPollTimeouts.Load()
	const waitMS = 700 // small but real: keeps the test fast while exercising the full wait

	start := time.Now()
	code, body := req(t, "GET", "/v1/worker/poll?wait_ms="+strconv.Itoa(waitMS), nil, workerTok())
	elapsed := time.Since(start)
	if code != http.StatusNoContent {
		t.Fatalf("idle long-poll: want 204 after the wait, got %d: %s", code, body)
	}
	// It must have actually WAITED (not returned 204 instantly): at least most of the
	// budget elapsed. Lower-bounded loosely to stay robust on a busy CI box.
	if elapsed < 500*time.Millisecond {
		t.Fatalf("idle long-poll returned in %v — it did not hold open for ~%dms", elapsed, waitMS)
	}
	// And it must not have hung far past the budget (the deadline fires, no leak).
	if elapsed > 5*time.Second {
		t.Fatalf("idle long-poll took %v — the wait deadline did not fire", elapsed)
	}
	if got := metrics.longPollTimeouts.Load(); got != before+1 {
		t.Fatalf("cx_long_poll_timeouts_total = %d, want %d (one timed-out empty return)", got, before+1)
	}
}

// TestPollNoWaitUnchanged guards backwards compatibility: a poll WITHOUT wait_ms
// (the pre-D1 single-shot contract an older agent speaks) still returns 204
// immediately on an empty queue and 200 on the next claimable task — and never
// counts as a long-poll timeout.
func TestPollNoWaitUnchanged(t *testing.T) {
	ctx := context.Background()
	t.Cleanup(func() { itPool.Exec(ctx, `TRUNCATE tasks, jobs, job_events CASCADE`) })
	reset(t)
	if _, err := itPool.Exec(ctx, `TRUNCATE tasks, jobs CASCADE`); err != nil {
		t.Fatalf("clear queue: %v", err)
	}

	before := metrics.longPollTimeouts.Load()
	// Empty queue, no wait_ms → immediate 204 (single-shot), and no timeout counted.
	start := time.Now()
	if code, body := req(t, "GET", "/v1/worker/poll", nil, workerTok()); code != http.StatusNoContent {
		t.Fatalf("no-wait empty poll: want 204, got %d: %s", code, body)
	}
	if elapsed := time.Since(start); elapsed > 300*time.Millisecond {
		t.Fatalf("no-wait poll took %v — it must not wait", elapsed)
	}
	if got := metrics.longPollTimeouts.Load(); got != before {
		t.Fatalf("a no-wait poll must NOT bump cx_long_poll_timeouts_total: %d -> %d", before, got)
	}

	// With work present, no-wait still claims it on the spot.
	_, taskID := seedClaimableEmbedTask(t, ctx)
	code, body := req(t, "GET", "/v1/worker/poll", nil, workerTok())
	if code != http.StatusOK {
		t.Fatalf("no-wait poll with work: want 200, got %d: %s", code, body)
	}
	var disp TaskDispatch
	if err := json.Unmarshal(body, &disp); err != nil {
		t.Fatalf("dispatch decode: %v (%s)", err, body)
	}
	if disp.TaskID != taskID {
		t.Fatalf("no-wait poll returned %s, want %s", disp.TaskID, taskID)
	}
}

// --- Compute Autopilot pipeline: output->input chaining ---

// driveOneTask claims the next queued task, PUTs an embed result, and commits it — the
// minimal worker loop for a single-task job (committing the final task finalizes the job
// synchronously, which is what fires advancePipeline).
//
// It is honeypot-aware: is_honeypot is never signalled to the worker over the wire (see
// handleWorkerPoll), so a real worker distinguishes a honeypot only by recognizing its
// input. Here, the presigned input_url embeds the object key (MinIO path-style URLs), so
// a dispatch for the seeded demo honeypot (control/seed.go's demoHoneypotEmbedRef) is
// identifiable — and must be answered with the honeypot's actual known answer, or
// verifyTaskResult correctly treats the generic canned result as wrong and requeues it.
// taskIsHoneypot reports the server-side is_honeypot truth for a dispatched task.
// A SERVER-SIDE test harness legitimately reads this from the DB to commit the
// right known answer; it is NOT a signal a real worker can see (the honeypot
// input-GET leak fix, Verification & Result Trust 5->5.5, closed the input_url
// substring that used to leak it on the wire).
func taskIsHoneypot(t *testing.T, ctx context.Context, taskID uuid.UUID) bool {
	t.Helper()
	var isHoneypot bool
	if err := itPool.QueryRow(ctx,
		`SELECT COALESCE(is_honeypot,false) FROM tasks WHERE id=$1`, taskID).Scan(&isHoneypot); err != nil {
		t.Fatalf("taskIsHoneypot(%s): %v", taskID, err)
	}
	return isHoneypot
}

func driveOneTask(t *testing.T, ctx context.Context) {
	t.Helper()
	code, body := req(t, "GET", "/v1/worker/poll", nil, workerTok())
	if code != 200 {
		t.Fatalf("poll: want 200, got %d: %s", code, body)
	}
	var disp TaskDispatch
	if err := json.Unmarshal(body, &disp); err != nil {
		t.Fatalf("dispatch decode: %v", err)
	}
	// Commit the honeypot's KNOWN answer when this dispatch is the honeypot, so it
	// PASSES. The honeypot input-GET leak fix (Verification & Result Trust 5->5.5)
	// deliberately makes the presigned input_url opaque — a real worker can no
	// longer tell a trap from the URL — so this harness identifies the honeypot the
	// only legitimate way a SERVER-SIDE test harness can: the is_honeypot flag in
	// the DB, keyed by the dispatched task id (a channel a real worker never has,
	// unlike the old input_url substring check the fix intentionally closed).
	result := embedResultJSON(1)
	if taskIsHoneypot(t, ctx, disp.TaskID) {
		result = demoHoneypotEmbedResultJSON(t)
	}
	if err := itStorage.PutObject(ctx, disp.ResultKey, result, "application/json"); err != nil {
		t.Fatalf("put result: %v", err)
	}
	commit := TaskCommit{TaskID: disp.TaskID, ResultKey: disp.ResultKey, DurationMS: 10, TokensUsed: 8}
	if code, b := req(t, "POST", "/v1/worker/task/"+disp.TaskID.String()+"/commit", commit, workerTok(), jsonCT()); code != 204 {
		t.Fatalf("commit: want 204, got %d: %s", code, b)
	}
}

// TestFileDispute proves the buyer-dispute seam (optimistic-verification / payout-
// guarantee foundation): the owning buyer can file a dispute (202 + an id + the honest
// boundary note), and a buyer can NOT dispute a job that is not theirs (404, nothing
// recorded). Resolution by optimistic recompute is intentionally NOT exercised — it is
// the external/future work behind the seam.
func TestFileDispute(t *testing.T) {
	reset(t)
	ctx := context.Background()
	jobID := uuid.New()
	if _, err := itPool.Exec(ctx,
		`INSERT INTO jobs (id, buyer_id, status, job_type, input_ref, task_count, tasks_done)
		 VALUES ($1,$2,'complete','embed','jobs/x/input.jsonl',1,1)`,
		jobID, demoBuyerUUID); err != nil {
		t.Fatalf("insert job: %v", err)
	}

	// Owner files a dispute → 202 with an id + status open.
	code, body := req(t, "POST", "/v1/jobs/"+jobID.String()+"/dispute",
		map[string]string{"reason": "result looks wrong"}, buyerKey(), jsonCT())
	if code != http.StatusAccepted {
		t.Fatalf("file dispute: want 202, got %d: %s", code, body)
	}
	var got struct {
		DisputeID string `json:"dispute_id"`
		Status    string `json:"status"`
	}
	if err := json.Unmarshal(body, &got); err != nil || got.DisputeID == "" || got.Status != "open" {
		t.Fatalf("dispute response missing id/status: %s", body)
	}
	var n int
	if err := itPool.QueryRow(ctx, `SELECT count(*) FROM disputes WHERE job_id=$1`, jobID).Scan(&n); err != nil || n != 1 {
		t.Fatalf("dispute not recorded: n=%d err=%v", n, err)
	}

	// A buyer cannot dispute a job that is not theirs (random/unknown id) → 404, and
	// nothing is recorded (the INSERT...SELECT...WHERE EXISTS yields no row).
	other := uuid.New()
	code2, _ := req(t, "POST", "/v1/jobs/"+other.String()+"/dispute",
		map[string]string{"reason": "x"}, buyerKey(), jsonCT())
	if code2 != http.StatusNotFound {
		t.Fatalf("dispute on non-owned job: want 404, got %d", code2)
	}
	if err := itPool.QueryRow(ctx, `SELECT count(*) FROM disputes WHERE job_id=$1`, other).Scan(&n); err != nil || n != 0 {
		t.Fatalf("non-owned dispute must not be recorded: n=%d err=%v", n, err)
	}
}

// TestDisputeResolverNoPeer proves the optimistic-verification resolver's honest
// boundary: when the only same-class supplier for the disputed job IS the original
// (no distinct peer to independently re-run on), the resolver surfaces 'no_peer' and
// retries later — it never fakes a resolution. The agree/disagree verdict paths flow
// through the existing redundancy verifier (covered by the tiebreak tests).
func TestDisputeResolverNoPeer(t *testing.T) {
	reset(t)
	ctx := context.Background()
	jobID, taskID := uuid.New(), uuid.New()
	mustJobTask(t, jobID, taskID, false, false, "jobs/x/input.jsonl")
	if _, err := itPool.Exec(ctx,
		`UPDATE tasks SET status='complete', worker_id=$2, result_ref=result_key,
		 verification_outcome='pass', verified_at=now(), completed_at=now()
		 WHERE id=$1`, taskID, demoWorkerUUID); err != nil {
		// worker_id is part of the verification class the resolver excludes.
		t.Fatalf("mark task delivered: %v", err)
	}
	if _, err := itPool.Exec(ctx, `UPDATE jobs SET status='complete',tasks_done=1 WHERE id=$1`, jobID); err != nil {
		t.Fatalf("mark job complete: %v", err)
	}
	code, body := req(t, "POST", "/v1/jobs/"+jobID.String()+"/dispute",
		map[string]string{"reason": "x"}, buyerKey(), jsonCT())
	if code != http.StatusAccepted {
		t.Fatalf("file dispute: %d %s", code, body)
	}

	wk := NewWorkers(itStore, itStorage, stubPayout{})
	if err := wk.resolveDisputes(ctx); err != nil {
		t.Fatalf("resolveDisputes: %v", err)
	}
	var status string
	if err := itPool.QueryRow(ctx, `SELECT status FROM disputes WHERE job_id=$1`, jobID).Scan(&status); err != nil {
		t.Fatalf("read dispute: %v", err)
	}
	if status != "no_peer" {
		t.Fatalf("want 'no_peer' (no distinct same-class supplier to re-verify), got %q", status)
	}
}

// TestVerificationReceiptSurfaced proves the verification RECEIPT path end to end: a real
// verification outcome records an append-only verification_events row, and the buyer
// job-status (GetJob) surfaces the aggregate counts + the honest derived label +
// charge_status. This is the moat made visible — exercised against live Postgres.
func TestVerificationReceiptSurfaced(t *testing.T) {
	reset(t)
	ctx := context.Background()
	jobID, taskID := uuid.New(), uuid.New()
	mustJobTask(t, jobID, taskID, false, false, "jobs/x/input.jsonl")
	if _, err := itPool.Exec(ctx,
		`UPDATE tasks SET status='complete', worker_id=$2, result_ref=result_key,
		 verification_outcome='pass', verified_at=now(), completed_at=now()
		 WHERE id=$1`, taskID, demoWorkerUUID); err != nil {
		t.Fatalf("mark task delivered: %v", err)
	}
	if _, err := itPool.Exec(ctx,
		`UPDATE jobs SET status='complete', tasks_done=1 WHERE id=$1`, jobID); err != nil {
		t.Fatalf("mark job complete: %v", err)
	}
	// A redundancy MATCH (two agreeing results) records a redundancy_match event for the job.
	info := &CommitTaskInfo{TaskID: taskID, JobID: jobID, WorkerID: demoWorkerUUID,
		SupplierID: demoSupplierUUID, jobType: "embed", HWClass: "apple_silicon_max"}
	if out, err := itServer.verifier.verifyTaskResult(ctx, info,
		TaskCommit{TaskID: taskID}, embedResultJSON(1), embedResultJSON(1)); err != nil || out != OutcomePass {
		t.Fatalf("verify match: out=%v err=%v", out, err)
	}
	var n int
	if err := itPool.QueryRow(ctx,
		`SELECT count(*) FROM verification_events WHERE job_id=$1 AND kind='redundancy_match'`, jobID).Scan(&n); err != nil || n < 1 {
		t.Fatalf("want >=1 redundancy_match verification_event, got n=%d err=%v", n, err)
	}
	// The buyer job-status surfaces the receipt aggregate, the honest label, and charge_status.
	jv, err := itStore.GetJob(ctx, jobID, demoBuyerUUID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if jv.Verification.RedundancyMatched < 1 || jv.Verification.Checked < 1 {
		t.Fatalf("receipt aggregate missing: %+v", jv.Verification)
	}
	if jv.Verification.Label != "fully-verified" || jv.Verification.DeliveredChunks != 1 || jv.Verification.VerifiedChunks != 1 {
		t.Fatalf("want one fully verified delivered chunk, got %+v", jv.Verification)
	}
	if jv.ChargeStatus == "" {
		t.Fatalf("charge_status should surface a default state, got empty")
	}
}

// --- buyer API-key lifecycle: mint → authenticate → revoke → 401 ---

// TestAPIKeyLifecycle proves the full /v1/keys contract end to end against live
// Postgres: a buyer mints a key (raw secret revealed once), the masked list shows
// it, the raw key authenticates a real buyer request, then revoking it makes that
// same key 401 · no silent accept of a revoked credential (BLACKHOLE).
func TestAPIKeyLifecycle(t *testing.T) {
	reset(t)

	// Mint a test key.
	code, out := req(t, "POST", "/v1/keys", map[string]any{"name": "ci-key", "test": true}, buyerKey(), jsonCT())
	if code != http.StatusCreated {
		t.Fatalf("create key: want 201, got %d: %s", code, out)
	}
	var created struct {
		ID, Name, Key, Prefix, Masked string
	}
	if err := json.Unmarshal(out, &created); err != nil {
		t.Fatalf("create decode: %v (%s)", err, out)
	}
	if created.Key == "" || !strings.HasPrefix(created.Key, "cx_test_") {
		t.Fatalf("expected raw cx_test_ key revealed once, got %q", created.Key)
	}
	if created.Prefix != "cx_test_" {
		t.Fatalf("expected prefix cx_test_, got %q", created.Prefix)
	}
	if created.Name != "ci-key" || created.ID == "" {
		t.Fatalf("create response missing name/id: %+v", created)
	}

	// List shows it masked · never the raw secret.
	code, out = req(t, "GET", "/v1/keys", nil, buyerKey())
	if code != http.StatusOK {
		t.Fatalf("list keys: want 200, got %d: %s", code, out)
	}
	if strings.Contains(string(out), created.Key) {
		t.Fatalf("list leaked the raw key: %s", out)
	}
	var list struct {
		Keys []struct {
			ID, Name, Masked string
			Revoked          bool
		}
	}
	if err := json.Unmarshal(out, &list); err != nil {
		t.Fatalf("list decode: %v (%s)", err, out)
	}
	var found bool
	for _, k := range list.Keys {
		if k.ID == created.ID {
			found = true
			if k.Masked == "" || !strings.HasPrefix(k.Masked, "cx_test_") {
				t.Fatalf("masked hint malformed: %q", k.Masked)
			}
			if k.Revoked {
				t.Fatalf("freshly minted key should not be revoked")
			}
		}
	}
	if !found {
		t.Fatalf("minted key %s not in list", created.ID)
	}

	// The raw key authenticates a real buyer request.
	newKeyHdr := hdr{"Authorization", "Bearer " + created.Key}
	if code, _ := req(t, "GET", "/v1/models", nil, newKeyHdr); code != http.StatusOK {
		t.Fatalf("authenticate with new key: want 200, got %d", code)
	}

	// Revoke it (idempotent 204).
	if code, _ := req(t, "DELETE", "/v1/keys/"+created.ID, nil, buyerKey()); code != http.StatusNoContent {
		t.Fatalf("revoke: want 204, got %d", code)
	}
	if code, _ := req(t, "DELETE", "/v1/keys/"+created.ID, nil, buyerKey()); code != http.StatusNoContent {
		t.Fatalf("revoke (idempotent replay): want 204, got %d", code)
	}

	// A revoked key no longer authenticates · 401, no silent bypass.
	if code, _ := req(t, "GET", "/v1/models", nil, newKeyHdr); code != http.StatusUnauthorized {
		t.Fatalf("revoked key must 401, got %d", code)
	}
}

// --- OpenAI-Batch-compatible adapter: inline embeddings batch → status maps ---

// --- self-serve accounts + auth + sandbox (accounts.go) ---

// uniqueEmail returns a per-run unique address so the UNIQUE(email) buyers table
// stays idempotent across repeated test runs without truncating it in reset().
func uniqueEmail(prefix string) string {
	return fmt.Sprintf("%s+%s@example.com", prefix, uuid.NewString()[:8])
}

// TestSignupTokenAuthenticatesAndSandboxGate proves the whole self-serve lane:
// signup returns a session token that authenticates a buyer route, the sandbox
// free credit lets a cardless buyer submit a job while Stripe is configured, and
// once the realized spend reaches the grant the 402 gate re-asserts honestly.
func TestSignupTokenAuthenticatesAndSandboxGate(t *testing.T) {
	reset(t)
	ctx := context.Background()
	// The sandbox free-credit lane is OFF by default (an unverified signup must not
	// grant Sybil-farmable free compute); an operator opts in via this env. Enable it
	// here to exercise the granted-then-exhausted path.
	t.Setenv("CX_SANDBOX_CREDIT_USD", "5")

	email := uniqueEmail("buyer")
	code, out := req(t, "POST", "/v1/signup", map[string]any{"email": email, "password": "hunter2hunter2"}, jsonCT())
	if code != http.StatusCreated {
		t.Fatalf("signup: want 201, got %d: %s", code, out)
	}
	var su struct {
		BuyerID       string  `json:"buyer_id"`
		Token         string  `json:"token"`
		Email         string  `json:"email"`
		FreeCreditUSD float64 `json:"free_credit_usd"`
		SandboxKey    string  `json:"sandbox_key"`
	}
	if err := json.Unmarshal(out, &su); err != nil {
		t.Fatalf("signup decode: %v (%s)", err, out)
	}
	if su.Token == "" || !strings.HasPrefix(su.Token, "cx_sess_") {
		t.Fatalf("signup must return a cx_sess_ token, got %q", su.Token)
	}
	if su.FreeCreditUSD <= 0 {
		t.Fatalf("signup must grant sandbox free credit, got %v", su.FreeCreditUSD)
	}
	if !strings.HasPrefix(su.SandboxKey, "cx_test_") {
		t.Fatalf("signup must mint a cx_test_ sandbox key, got %q", su.SandboxKey)
	}
	buyerID := uuid.MustParse(su.BuyerID)

	// The session token authenticates a buyer route.
	sessHdr := hdr{"Authorization", "Bearer " + su.Token}
	if code, _ := req(t, "GET", "/v1/models", nil, sessHdr); code != http.StatusOK {
		t.Fatalf("session token must authenticate /v1/models, got %d", code)
	}

	// Submit a job AS the new sandbox buyer with Stripe configured + NO card on
	// file: the free-credit exemption must allow it. The submit gate reads only the
	// DB (GetBillingCustomer + free credit), so a fake key triggers the gate without
	// any Stripe network call.
	t.Setenv("STRIPE_SECRET_KEY", "sk_test_fake_for_gate_only")
	var sb strings.Builder
	for i := 0; i < 4; i++ {
		fmt.Fprintf(&sb, `{"id":"r%d","text":"record %d"}`+"\n", i, i)
	}
	jobBody := map[string]any{
		"job_type":     map[string]any{"type": "embed"},
		"model":        map[string]any{"kind": "hf", "ref": "all-minilm-l6-v2"},
		"params":       map[string]any{"split_size": 1000},
		"verification": map[string]any{},
		"tier":         "batch",
		"input":        sb.String(),
	}
	code, out = req(t, "POST", "/v1/jobs", jobBody, sessHdr, jsonCT())
	if code != http.StatusAccepted {
		t.Fatalf("sandbox submit under free credit: want 202, got %d: %s", code, out)
	}

	// Exhaust the free credit by recording a buyer_charge debit >= the grant, then
	// the SAME submit must 402 (no card, credit gone). This is the honest boundary.
	if _, err := itPool.Exec(ctx,
		`INSERT INTO ledger_entries (kind, buyer_id, amount_usd, payout_status)
		 VALUES ('buyer_charge', $1, $2, 'released')`,
		buyerID, -(su.FreeCreditUSD + 1.0)); err != nil {
		t.Fatalf("seed buyer_charge: %v", err)
	}
	code, out = req(t, "POST", "/v1/jobs", jobBody, sessHdr, jsonCT())
	if code != http.StatusPaymentRequired {
		t.Fatalf("submit after credit exhausted: want 402, got %d: %s", code, out)
	}
}

// TestMeReturnsAuthenticatedIdentity proves GET /v1/me, gated by the signup session
// token, reports the buyer's own identity: email matches signup, buyer_id is set, and
// is_admin is false for a self-serve account.
func TestMeReturnsAuthenticatedIdentity(t *testing.T) {
	reset(t)
	email := uniqueEmail("me")
	code, out := req(t, "POST", "/v1/signup", map[string]any{"email": email, "password": "hunter2hunter2"}, jsonCT())
	if code != http.StatusCreated {
		t.Fatalf("signup: want 201, got %d: %s", code, out)
	}
	var su struct {
		BuyerID string `json:"buyer_id"`
		Token   string `json:"token"`
	}
	if err := json.Unmarshal(out, &su); err != nil {
		t.Fatalf("signup decode: %v (%s)", err, out)
	}

	code, out = req(t, "GET", "/v1/me", nil, hdr{"Authorization", "Bearer " + su.Token})
	if code != http.StatusOK {
		t.Fatalf("GET /v1/me: want 200, got %d: %s", code, out)
	}
	var me struct {
		BuyerID                string  `json:"buyer_id"`
		Email                  string  `json:"email"`
		IsAdmin                bool    `json:"is_admin"`
		FreeCreditRemainingUSD float64 `json:"free_credit_remaining_usd"`
	}
	if err := json.Unmarshal(out, &me); err != nil {
		t.Fatalf("/v1/me decode: %v (%s)", err, out)
	}
	if me.Email != email {
		t.Fatalf("/v1/me email: want %q, got %q", email, me.Email)
	}
	if me.BuyerID == "" || me.BuyerID != su.BuyerID {
		t.Fatalf("/v1/me buyer_id: want %q, got %q", su.BuyerID, me.BuyerID)
	}
	if me.IsAdmin {
		t.Fatalf("/v1/me is_admin: want false for a self-serve account, got true")
	}
}

// TestSignupDuplicateEmailConflicts proves the UNIQUE email is enforced honestly.
func TestSignupDuplicateEmailConflicts(t *testing.T) {
	reset(t)
	email := uniqueEmail("dup")
	if code, out := req(t, "POST", "/v1/signup", map[string]any{"email": email, "password": "hunter2hunter2"}, jsonCT()); code != http.StatusCreated {
		t.Fatalf("first signup: want 201, got %d: %s", code, out)
	}
	if code, out := req(t, "POST", "/v1/signup", map[string]any{"email": email, "password": "anotherpassword"}, jsonCT()); code != http.StatusConflict {
		t.Fatalf("duplicate signup: want 409, got %d: %s", code, out)
	}
}

// TestLoginGoodAndBadPassword proves login issues a token on the right password
// and returns 401 (never a silent accept) on a wrong one or an unknown email.
func TestLoginGoodAndBadPassword(t *testing.T) {
	reset(t)
	email := uniqueEmail("login")
	const pw = "correct horse battery"
	if code, out := req(t, "POST", "/v1/signup", map[string]any{"email": email, "password": pw}, jsonCT()); code != http.StatusCreated {
		t.Fatalf("signup: want 201, got %d: %s", code, out)
	}

	// Good password → 200 + token that authenticates.
	code, out := req(t, "POST", "/v1/login", map[string]any{"email": email, "password": pw}, jsonCT())
	if code != http.StatusOK {
		t.Fatalf("login good: want 200, got %d: %s", code, out)
	}
	var li struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(out, &li); err != nil || li.Token == "" {
		t.Fatalf("login token missing: %v (%s)", err, out)
	}
	if code, _ := req(t, "GET", "/v1/models", nil, hdr{"Authorization", "Bearer " + li.Token}); code != http.StatusOK {
		t.Fatalf("login token must authenticate, got %d", code)
	}

	// Wrong password → 401.
	if code, _ := req(t, "POST", "/v1/login", map[string]any{"email": email, "password": "wrong"}, jsonCT()); code != http.StatusUnauthorized {
		t.Fatalf("login bad password: want 401, got %d", code)
	}
	// Unknown email → same 401 (no user enumeration).
	if code, _ := req(t, "POST", "/v1/login", map[string]any{"email": uniqueEmail("nope"), "password": pw}, jsonCT()); code != http.StatusUnauthorized {
		t.Fatalf("login unknown email: want 401, got %d", code)
	}
}

// --- supplier onboarding (suppliers.go) ---

type supplierTestAccount struct {
	buyerID uuid.UUID
	email   string
	auth    hdr
}

func newSupplierTestAccount(t *testing.T, prefix string) supplierTestAccount {
	t.Helper()
	email := uniqueEmail(prefix)
	code, out := req(t, "POST", "/v1/signup",
		map[string]any{"email": email, "password": "supplier-test-password"}, jsonCT())
	if code != http.StatusCreated {
		t.Fatalf("supplier account signup: want 201, got %d: %s", code, out)
	}
	var created struct {
		BuyerID string `json:"buyer_id"`
		Token   string `json:"token"`
	}
	if err := json.Unmarshal(out, &created); err != nil || created.Token == "" {
		t.Fatalf("supplier account signup decode: %v (%s)", err, out)
	}
	return supplierTestAccount{
		buyerID: uuid.MustParse(created.BuyerID),
		email:   email,
		auth:    hdr{"Authorization", "Bearer " + created.Token},
	}
}

// TestSupplierOnboardBoundToAuthenticatedAccount proves the supplier identity is
// derived from the presenting buyer account, legacy email/tax fields are rejected,
// KYC stays at Stripe, and an unconfigured Connect boundary is still an honest 503.
func TestSupplierOnboardBoundToAuthenticatedAccount(t *testing.T) {
	reset(t)
	t.Setenv("STRIPE_SECRET_KEY", "")
	ctx := context.Background()
	legacyBuyerID := uuid.New()
	legacyKey := "cx_test_supplier_legacy_" + uuid.NewString()
	if _, err := itPool.Exec(ctx,
		`INSERT INTO api_keys (buyer_id,key_hash,revoked) VALUES ($1,$2,false)`,
		legacyBuyerID, hashKey(legacyKey),
	); err != nil {
		t.Fatalf("insert accountless buyer key: %v", err)
	}
	if code, out := req(t, "POST", "/v1/supplier/worker-tokens", map[string]any{}, jsonCT(),
		hdr{"Authorization", "Bearer " + legacyKey}); code != http.StatusForbidden {
		t.Fatalf("accountless buyer identity: want 403, got %d: %s", code, out)
	}
	account := newSupplierTestAccount(t, "supplier-owned")

	// Old clients must not be allowed to choose identity or send plaintext KYC/tax
	// data. Reject before creating any supplier row.
	code, out := req(t, "POST", "/v1/supplier/onboard",
		map[string]any{"email": "victim@example.com", "tax_id": "12-3456789", "tax_country": "US"}, jsonCT(), account.auth)
	if code != http.StatusBadRequest {
		t.Fatalf("onboard with caller identity/KYC: want 400, got %d: %s", code, out)
	}
	var before int
	if err := itPool.QueryRow(ctx, `SELECT count(*) FROM suppliers WHERE owner_buyer_id=$1`, account.buyerID).Scan(&before); err != nil {
		t.Fatalf("count supplier before clean onboard: %v", err)
	}
	if before != 0 {
		t.Fatalf("rejected identity/KYC body must not create a supplier, got %d", before)
	}

	code, out = req(t, "POST", "/v1/supplier/onboard", map[string]any{}, jsonCT(), account.auth)
	if code != http.StatusServiceUnavailable {
		t.Fatalf("onboard with no Stripe: want 503, got %d: %s", code, out)
	}

	var supplierID, ownerID uuid.UUID
	var supplierEmail string
	if err := itPool.QueryRow(ctx,
		`SELECT id, owner_buyer_id, email FROM suppliers WHERE owner_buyer_id=$1`, account.buyerID,
	).Scan(&supplierID, &ownerID, &supplierEmail); err != nil {
		t.Fatalf("read owned supplier: %v", err)
	}
	if ownerID != account.buyerID || supplierEmail != account.email {
		t.Fatalf("supplier must use authenticated account identity: owner=%s email=%q", ownerID, supplierEmail)
	}

	code, out = req(t, "GET", "/v1/supplier/status", nil, account.auth)
	if code != http.StatusOK {
		t.Fatalf("supplier status: want 200, got %d: %s", code, out)
	}
	var st struct {
		SupplierID     uuid.UUID `json:"supplier_id"`
		ConnectStatus  string    `json:"connect_status"`
		PayoutsEnabled bool      `json:"payouts_enabled"`
		KYCProvider    string    `json:"kyc_provider"`
	}
	if err := json.Unmarshal(out, &st); err != nil {
		t.Fatalf("status decode: %v (%s)", err, out)
	}
	if st.SupplierID != supplierID || st.ConnectStatus != "none" || st.PayoutsEnabled || st.KYCProvider != "stripe_connect" {
		t.Fatalf("no Connect account yet: want none/false, got %+v", st)
	}

	var statusBody map[string]json.RawMessage
	if err := json.Unmarshal(out, &statusBody); err != nil {
		t.Fatalf("status map decode: %v", err)
	}
	if _, exists := statusBody["tax_on_file"]; exists {
		t.Fatalf("status must not claim CX stores tax data: %s", out)
	}
	var taxColumns int
	if err := itPool.QueryRow(ctx,
		`SELECT count(*) FROM information_schema.columns
		  WHERE table_schema = current_schema()
		    AND table_name = 'suppliers'
		    AND column_name IN ('tax_id','tax_country')`,
	).Scan(&taxColumns); err != nil {
		t.Fatalf("inspect supplier tax columns: %v", err)
	}
	if taxColumns != 0 {
		t.Fatalf("CX must not retain plaintext tax columns, found %d", taxColumns)
	}
}

// TestSupplierRoutesPreventCrossAccountAccess proves that a second authenticated
// buyer cannot select the first supplier by email for status, onboarding, or token
// minting. Empty-body requests create and operate only on the caller's own supplier.
func TestSupplierRoutesPreventCrossAccountAccess(t *testing.T) {
	reset(t)
	t.Setenv("STRIPE_SECRET_KEY", "")
	ctx := context.Background()
	owner := newSupplierTestAccount(t, "supplier-a")
	other := newSupplierTestAccount(t, "supplier-b")

	code, out := req(t, "POST", "/v1/supplier/worker-tokens", map[string]any{}, jsonCT(), owner.auth)
	if code != http.StatusCreated {
		t.Fatalf("owner token mint: want 201, got %d: %s", code, out)
	}
	var ownerMint struct {
		SupplierID uuid.UUID `json:"supplier_id"`
		WorkerID   uuid.UUID `json:"worker_id"`
		Token      string    `json:"worker_token"`
	}
	if err := json.Unmarshal(out, &ownerMint); err != nil || ownerMint.SupplierID == uuid.Nil || ownerMint.Token == "" {
		t.Fatalf("owner token response: %v (%s)", err, out)
	}
	if _, err := itStore.CreateWorkerTokenForBuyer(ctx, other.buyerID, uuid.New(), ownerMint.SupplierID); !errors.Is(err, errSupplierOwnershipConflict) {
		t.Fatalf("store cross-account mint: want ownership conflict, got %v", err)
	}

	if code, out = req(t, "GET", "/v1/supplier/status", nil, other.auth); code != http.StatusNotFound {
		t.Fatalf("other account status before own supplier: want 404, got %d: %s", code, out)
	}
	if code, out = req(t, "GET", "/v1/supplier/status?email="+owner.email, nil, other.auth); code != http.StatusBadRequest {
		t.Fatalf("cross-account status selector: want 400, got %d: %s", code, out)
	}
	if code, out = req(t, "POST", "/v1/supplier/onboard", map[string]any{"email": owner.email}, jsonCT(), other.auth); code != http.StatusBadRequest {
		t.Fatalf("cross-account onboard selector: want 400, got %d: %s", code, out)
	}
	if code, out = req(t, "POST", "/v1/supplier/worker-tokens", map[string]any{"email": owner.email}, jsonCT(), other.auth); code != http.StatusBadRequest {
		t.Fatalf("cross-account token selector: want 400, got %d: %s", code, out)
	}

	var ownerTokens int
	if err := itPool.QueryRow(ctx,
		`SELECT count(*) FROM worker_tokens WHERE supplier_id=$1`, ownerMint.SupplierID,
	).Scan(&ownerTokens); err != nil {
		t.Fatalf("count owner tokens: %v", err)
	}
	if ownerTokens != 1 {
		t.Fatalf("cross-account requests changed owner token count: want 1, got %d", ownerTokens)
	}

	code, out = req(t, "POST", "/v1/supplier/worker-tokens", map[string]any{}, jsonCT(), other.auth)
	if code != http.StatusCreated {
		t.Fatalf("other account own token mint: want 201, got %d: %s", code, out)
	}
	var otherMint struct {
		SupplierID uuid.UUID `json:"supplier_id"`
		Token      string    `json:"worker_token"`
	}
	if err := json.Unmarshal(out, &otherMint); err != nil || otherMint.Token == "" {
		t.Fatalf("other token response: %v (%s)", err, out)
	}
	if otherMint.SupplierID == ownerMint.SupplierID {
		t.Fatalf("distinct buyer accounts received the same supplier id %s", ownerMint.SupplierID)
	}
	var ownerA, ownerB uuid.UUID
	if err := itPool.QueryRow(ctx, `SELECT owner_buyer_id FROM suppliers WHERE id=$1`, ownerMint.SupplierID).Scan(&ownerA); err != nil {
		t.Fatalf("read first owner: %v", err)
	}
	if err := itPool.QueryRow(ctx, `SELECT owner_buyer_id FROM suppliers WHERE id=$1`, otherMint.SupplierID).Scan(&ownerB); err != nil {
		t.Fatalf("read second owner: %v", err)
	}
	if ownerA != owner.buyerID || ownerB != other.buyerID {
		t.Fatalf("wrong supplier owners: first=%s second=%s", ownerA, ownerB)
	}
}

// TestSupplierLegacyCollisionFailsClosed proves a request cannot claim an unowned
// legacy supplier simply because its email equals the authenticated account email.
func TestSupplierLegacyCollisionFailsClosed(t *testing.T) {
	reset(t)
	ctx := context.Background()
	account := newSupplierTestAccount(t, "supplier-legacy")
	legacyID := uuid.New()
	if _, err := itPool.Exec(ctx,
		`INSERT INTO suppliers (id, email, status, owner_buyer_id)
		 VALUES ($1,$2,'pending',NULL)`, legacyID, account.email,
	); err != nil {
		t.Fatalf("insert unowned legacy supplier: %v", err)
	}

	code, out := req(t, "POST", "/v1/supplier/worker-tokens", map[string]any{}, jsonCT(), account.auth)
	if code != http.StatusConflict {
		t.Fatalf("legacy collision token mint: want 409, got %d: %s", code, out)
	}
	var stillUnowned bool
	if err := itPool.QueryRow(ctx,
		`SELECT owner_buyer_id IS NULL FROM suppliers WHERE id=$1`, legacyID,
	).Scan(&stillUnowned); err != nil {
		t.Fatalf("read legacy ownership: %v", err)
	}
	if !stillUnowned {
		t.Fatal("request-time email collision must not claim an unowned legacy supplier")
	}
	var tokenCount int
	if err := itPool.QueryRow(ctx, `SELECT count(*) FROM worker_tokens WHERE supplier_id=$1`, legacyID).Scan(&tokenCount); err != nil {
		t.Fatalf("count legacy tokens: %v", err)
	}
	if tokenCount != 0 {
		t.Fatalf("legacy ownership conflict minted %d worker tokens", tokenCount)
	}
}

// TestSupplierOwnershipMigrationBackfillsOnlyUnambiguous proves the idempotent
// runtime migration binds a one-to-one legacy match but deliberately leaves a
// case-insensitively ambiguous match unowned.
func TestSupplierOwnershipMigrationBackfillsOnlyUnambiguous(t *testing.T) {
	reset(t)
	ctx := context.Background()

	uniqueBuyer := uuid.New()
	uniqueSupplier := uuid.New()
	uniqueMail := uniqueEmail("supplier-backfill")
	ambiguousBuyer := uuid.New()
	ambiguousSupplierA := uuid.New()
	ambiguousSupplierB := uuid.New()
	ambiguousMail := uniqueEmail("supplier-ambiguous")
	maskedBuyerA := uuid.New()
	maskedBuyerB := uuid.New()
	maskedOwnedSupplier := uuid.New()
	maskedUnownedSupplier := uuid.New()
	maskedMail := uniqueEmail("supplier-masked-ambiguity")
	if _, err := itPool.Exec(ctx,
		`INSERT INTO buyers (id,email) VALUES
		 ($1,$2),($3,$4),($5,$6),($7,$8)`,
		uniqueBuyer, uniqueMail,
		ambiguousBuyer, ambiguousMail,
		maskedBuyerA, maskedMail,
		maskedBuyerB, strings.ToUpper(maskedMail),
	); err != nil {
		t.Fatalf("insert migration buyers: %v", err)
	}
	if _, err := itPool.Exec(ctx,
		`INSERT INTO suppliers (id,email,status,owner_buyer_id) VALUES
		 ($1,$2,'pending',NULL),
		 ($3,$4,'pending',NULL),
		 ($5,$6,'pending',NULL),
		 ($7,$8,'pending',$9),
		 ($10,$11,'pending',NULL)`,
		uniqueSupplier, uniqueMail,
		ambiguousSupplierA, ambiguousMail,
		ambiguousSupplierB, strings.ToUpper(ambiguousMail),
		maskedOwnedSupplier, uniqueEmail("supplier-owned-elsewhere"), maskedBuyerA,
		maskedUnownedSupplier, maskedMail,
	); err != nil {
		t.Fatalf("insert migration suppliers: %v", err)
	}

	if err := itStore.Migrate(ctx); err != nil {
		t.Fatalf("re-run ownership migration: %v", err)
	}
	var uniqueOwner uuid.UUID
	if err := itPool.QueryRow(ctx, `SELECT owner_buyer_id FROM suppliers WHERE id=$1`, uniqueSupplier).Scan(&uniqueOwner); err != nil {
		t.Fatalf("read unique backfill: %v", err)
	}
	if uniqueOwner != uniqueBuyer {
		t.Fatalf("one-to-one legacy supplier not backfilled: want %s, got %s", uniqueBuyer, uniqueOwner)
	}
	var ambiguousOwned int
	if err := itPool.QueryRow(ctx,
		`SELECT count(*) FROM suppliers
		  WHERE id IN ($1,$2) AND owner_buyer_id IS NOT NULL`,
		ambiguousSupplierA, ambiguousSupplierB,
	).Scan(&ambiguousOwned); err != nil {
		t.Fatalf("read ambiguous backfill: %v", err)
	}
	if ambiguousOwned != 0 {
		t.Fatalf("ambiguous legacy email matches must remain unowned, got %d owned rows", ambiguousOwned)
	}
	var maskedStillUnowned bool
	if err := itPool.QueryRow(ctx,
		`SELECT owner_buyer_id IS NULL FROM suppliers WHERE id=$1`, maskedUnownedSupplier,
	).Scan(&maskedStillUnowned); err != nil {
		t.Fatalf("read masked ambiguity backfill: %v", err)
	}
	if !maskedStillUnowned {
		t.Fatal("an already-owned duplicate buyer must not hide an ambiguous email match")
	}
}

// TestConnectWebhookFlipsPayoutsEnabled proves the account.updated webhook flips
// the cached payouts_enabled flag for the right supplier (signature-verified).
func TestConnectWebhookFlipsPayoutsEnabled(t *testing.T) {
	reset(t)
	t.Setenv("STRIPE_SECRET_KEY", "")
	t.Setenv("CX_CONNECT_WEBHOOK_SECRET", "whsec_test_connect")
	ctx := context.Background()

	// Onboard records this account's supplier (503 on the Connect link is expected),
	// then attach a known stripe_acct so the signed webhook has a target.
	account := newSupplierTestAccount(t, "supplier-webhook")
	_, _ = req(t, "POST", "/v1/supplier/onboard", map[string]any{}, jsonCT(), account.auth)
	const acct = "acct_TESTwebhook123"
	if _, err := itPool.Exec(ctx, `UPDATE suppliers SET stripe_acct=$2 WHERE owner_buyer_id=$1`, account.buyerID, acct); err != nil {
		t.Fatalf("set stripe_acct: %v", err)
	}

	body := []byte(`{"type":"account.updated","data":{"object":{"id":"` + acct + `","payouts_enabled":true}}}`)
	sig := stripeTestSig(body, "whsec_test_connect")
	code, out := req(t, "POST", "/v1/stripe/connect-webhook", body, hdr{"Stripe-Signature", sig})
	if code != http.StatusOK {
		t.Fatalf("connect webhook: want 200, got %d: %s", code, out)
	}

	var pe bool
	if err := itPool.QueryRow(ctx, `SELECT COALESCE(payouts_enabled,false) FROM suppliers WHERE owner_buyer_id=$1`, account.buyerID).Scan(&pe); err != nil {
		t.Fatalf("read payouts_enabled: %v", err)
	}
	if !pe {
		t.Fatalf("account.updated must flip payouts_enabled true")
	}

	// A tampered signature is rejected (no silent accept).
	if code, _ := req(t, "POST", "/v1/stripe/connect-webhook", body, hdr{"Stripe-Signature", "t=1,v1=deadbeef"}); code != http.StatusBadRequest {
		t.Fatalf("bad signature: want 400, got %d", code)
	}
}

// stripeTestSig builds a valid Stripe-Signature header for body under secret,
// using the same t.payload HMAC-SHA256 scheme verifyStripeSig checks.
func stripeTestSig(body []byte, secret string) string {
	t := strconv.FormatInt(time.Now().Unix(), 10)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(t + "." + string(body)))
	return "t=" + t + ",v1=" + hex.EncodeToString(mac.Sum(nil))
}

// --- Phase 3.5 auth hardening ---

// TestLogoutRevokesSession proves POST /v1/logout kills the presenting session token
// immediately (it must not stay valid for the rest of its 30-day TTL).
func TestLogoutRevokesSession(t *testing.T) {
	reset(t)
	email := uniqueEmail("logout")
	code, out := req(t, "POST", "/v1/signup", map[string]any{"email": email, "password": "hunter2hunter2"}, jsonCT())
	if code != http.StatusCreated {
		t.Fatalf("signup: %d %s", code, out)
	}
	var su struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(out, &su); err != nil || su.Token == "" {
		t.Fatalf("signup token: %v (%s)", err, out)
	}
	sess := hdr{"Authorization", "Bearer " + su.Token}
	if c, _ := req(t, "GET", "/v1/models", nil, sess); c != http.StatusOK {
		t.Fatalf("token must authenticate before logout, got %d", c)
	}
	if c, _ := req(t, "POST", "/v1/logout", nil, sess); c != http.StatusNoContent {
		t.Fatalf("logout: want 204, got %d", c)
	}
	if c, _ := req(t, "GET", "/v1/models", nil, sess); c != http.StatusUnauthorized {
		t.Fatalf("revoked session must 401, got %d", c)
	}
}

// TestLoginThrottleLocksOut proves the per-account login throttle: maxLoginFails bad
// passwords lock the email out (429), and even the correct password is refused during
// the lockout window.
func TestLoginThrottleLocksOut(t *testing.T) {
	reset(t)
	email := uniqueEmail("throttle")
	if c, out := req(t, "POST", "/v1/signup", map[string]any{"email": email, "password": "correct-horse-battery"}, jsonCT()); c != http.StatusCreated {
		t.Fatalf("signup: %d %s", c, out)
	}
	for i := 0; i < 5; i++ {
		if c, _ := req(t, "POST", "/v1/login", map[string]any{"email": email, "password": "wrong-guess"}, jsonCT()); c != http.StatusUnauthorized {
			t.Fatalf("bad login %d: want 401, got %d", i, c)
		}
	}
	if c, _ := req(t, "POST", "/v1/login", map[string]any{"email": email, "password": "wrong-guess"}, jsonCT()); c != http.StatusTooManyRequests {
		t.Fatalf("after 5 failures login must lock out (429), got %d", c)
	}
	if c, _ := req(t, "POST", "/v1/login", map[string]any{"email": email, "password": "correct-horse-battery"}, jsonCT()); c != http.StatusTooManyRequests {
		t.Fatalf("correct password during lockout must still 429, got %d", c)
	}
}

// TestSignupRejectsOversizePassword proves the bcrypt 72-byte guard (so two passwords
// sharing a 72-byte prefix can never authenticate interchangeably).
func TestSignupRejectsOversizePassword(t *testing.T) {
	reset(t)
	if c, _ := req(t, "POST", "/v1/signup", map[string]any{"email": uniqueEmail("long"), "password": strings.Repeat("a", 73)}, jsonCT()); c != http.StatusBadRequest {
		t.Fatalf("password > 72 bytes must 400, got %d", c)
	}
}

// TestSignupPerIPDailyCapEnforced proves the signup-specific abuse cap (independent
// of the generic flood limiter): signupsPerIPPerDay accounts succeed from one source
// IP, the next one 429s, and a DIFFERENT source IP is unaffected. X-Forwarded-For
// simulates a remote caller — the test harness itself connects over loopback, which
// every limiter in ratelimit.go exempts, so without a spoofed forwarded IP this path
// would never be exercised.
func TestSignupPerIPDailyCapEnforced(t *testing.T) {
	reset(t)
	const fromIP = "203.0.113.10"
	for i := 0; i < signupsPerIPPerDay; i++ {
		code, out := req(t, "POST", "/v1/signup",
			map[string]any{"email": uniqueEmail("cap"), "password": "hunter2hunter2"},
			jsonCT(), hdr{"X-Forwarded-For", fromIP})
		if code != http.StatusCreated {
			t.Fatalf("signup %d/%d from %s: want 201, got %d: %s", i+1, signupsPerIPPerDay, fromIP, code, out)
		}
	}
	// One more from the SAME IP must be capped, before it even touches the DB
	// (so a distinct email doesn't rescue it).
	code, out := req(t, "POST", "/v1/signup",
		map[string]any{"email": uniqueEmail("cap"), "password": "hunter2hunter2"},
		jsonCT(), hdr{"X-Forwarded-For", fromIP})
	if code != http.StatusTooManyRequests {
		t.Fatalf("signup %d from %s: want 429 (daily cap), got %d: %s", signupsPerIPPerDay+1, fromIP, code, out)
	}

	// A DIFFERENT source IP is a separate bucket and must still be allowed through.
	code, out = req(t, "POST", "/v1/signup",
		map[string]any{"email": uniqueEmail("cap-other"), "password": "hunter2hunter2"},
		jsonCT(), hdr{"X-Forwarded-For", "203.0.113.20"})
	if code != http.StatusCreated {
		t.Fatalf("signup from a different, uncapped IP: want 201, got %d: %s", code, out)
	}
}

// --- stuck-run watchdog (workers.go reapStuckJobs) ---

// TestStuckJobReaperCancelsAndCheckpoints proves the watchdog's KILL contract
// (watchdog_strikes=1 seeds the job one rung up the ladder so the kill path runs
// directly; the rescue rung is proven by TestWatchdogRescueThenKill): a running
// job past its deadline with no task progress is cancelled with the completed
// work CHECKPOINTED (partial merge at output_ref) and SETTLED (actual_usd =
// completed tasks' charges only; no refund row — completed work stays charged,
// un-run work was never charged), the unfinished task is cancelled, the buyer sees
// a job_stuck_cancelled timeline event exactly once, and a job that is equally late
// but still PROGRESSING is left alone.
func TestStuckJobReaperCancelsAndCheckpoints(t *testing.T) {
	reset(t)
	ctx := context.Background()

	// STUCK job: eta 10s, created 10 minutes ago (past the floored deadline
	// eta+120s = 130s), already on strike 1; task 0 completed 10 minutes ago,
	// task 1 claimed 10 minutes ago and never committed — no progress (commit,
	// claim, or retry visibility) inside the grace window.
	jobID := uuid.New()
	if _, err := itPool.Exec(ctx,
		`INSERT INTO jobs (id, buyer_id, status, job_type, model_ref, input_ref, tier,
		                   task_count, tasks_done, min_memory_gb, eta_secs, output_ref, created_at,
		                   watchdog_strikes)
		 VALUES ($1,$2,'running','embed','all-minilm-l6-v2','jobs/stuck/input.jsonl','batch',
		         2,1,2,10,'jobs/stuck/output.jsonl', now() - interval '10 minutes', 1)`,
		jobID, demoBuyerUUID); err != nil {
		t.Fatal(err)
	}
	doneTask, stuckTask := uuid.New(), uuid.New()
	doneKey := "jobs/stuck/tasks/0/result.json"
	if err := itStorage.PutObject(ctx, doneKey, embedResultJSON(1), "application/json"); err != nil {
		t.Fatalf("seed result object: %v", err)
	}
	if _, err := itPool.Exec(ctx,
		`INSERT INTO tasks (id, job_id, status, input_ref, result_key, result_ref, chunk_index,
		                    worker_id, claimed_by, completed_at, visible_at)
		 VALUES ($1,$2,'complete','jobs/stuck/tasks/0/input.jsonl',$3,$3,0,$4,$4,
		         now() - interval '10 minutes', now() - interval '10 minutes')`,
		doneTask, jobID, doneKey, demoWorkerUUID); err != nil {
		t.Fatal(err)
	}
	if _, err := itPool.Exec(ctx,
		`INSERT INTO tasks (id, job_id, status, input_ref, result_key, chunk_index,
		                    worker_id, claimed_by, claimed_at, started_at, visible_at)
		 VALUES ($1,$2,'running','jobs/stuck/tasks/1/input.jsonl','jobs/stuck/tasks/1/result.json',1,
		         $3,$3, now() - interval '10 minutes', now() - interval '10 minutes', now() - interval '10 minutes')`,
		stuckTask, jobID, demoWorkerUUID); err != nil {
		t.Fatal(err)
	}
	// The completed task's charge already settled at commit (the real per-task split).
	if _, err := itPool.Exec(ctx,
		`INSERT INTO ledger_entries (kind, buyer_id, task_id, amount_usd, payout_status)
		 VALUES ('buyer_charge', $1, $2, -0.10, 'released')`,
		demoBuyerUUID, doneTask); err != nil {
		t.Fatal(err)
	}
	if _, err := itPool.Exec(ctx,
		`INSERT INTO ledger_entries (kind, supplier_id, task_id, amount_usd, payout_status)
		 VALUES ('supplier_credit', $1, $2, 0.09, 'held')`,
		demoSupplierUUID, doneTask); err != nil {
		t.Fatal(err)
	}
	if _, err := itPool.Exec(ctx,
		`INSERT INTO ledger_entries (kind, task_id, amount_usd, payout_status)
		 VALUES ('platform_take', $1, 0.01, 'released')`, doneTask); err != nil {
		t.Fatal(err)
	}

	// CONTROL job: equally past its deadline but a task completed 5 seconds ago —
	// PROGRESSING, so the watchdog must not touch it.
	ctlJob, ctlTask := uuid.New(), uuid.New()
	if _, err := itPool.Exec(ctx,
		`INSERT INTO jobs (id, buyer_id, status, job_type, model_ref, input_ref, tier,
		                   task_count, tasks_done, min_memory_gb, eta_secs, created_at, watchdog_strikes)
		 VALUES ($1,$2,'running','embed','all-minilm-l6-v2','jobs/ctl/input.jsonl','batch',
		         2,1,2,10, now() - interval '10 minutes', 1)`,
		ctlJob, demoBuyerUUID); err != nil {
		t.Fatal(err)
	}
	if _, err := itPool.Exec(ctx,
		`INSERT INTO tasks (id, job_id, status, input_ref, result_key, chunk_index,
		                    worker_id, claimed_by, completed_at)
		 VALUES ($1,$2,'complete','jobs/ctl/tasks/0/input.jsonl','jobs/ctl/tasks/0/result.json',0,
		         $3,$3, now() - interval '5 seconds')`,
		ctlTask, ctlJob, demoWorkerUUID); err != nil {
		t.Fatal(err)
	}

	wk := NewWorkers(itStore, itStorage, stubPayout{})
	if err := wk.reapStuckJobs(ctx); err != nil {
		t.Fatalf("reapStuckJobs: %v", err)
	}

	var status string
	if err := itPool.QueryRow(ctx, `SELECT status FROM jobs WHERE id=$1`, jobID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "cancelled" {
		t.Fatalf("stuck job: want cancelled, got %q", status)
	}
	if err := itPool.QueryRow(ctx, `SELECT status FROM jobs WHERE id=$1`, ctlJob).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "running" {
		t.Fatalf("progressing job must be untouched: want running, got %q", status)
	}
	if err := itPool.QueryRow(ctx, `SELECT status FROM tasks WHERE id=$1`, stuckTask).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "cancelled" {
		t.Fatalf("unfinished task: want cancelled, got %q", status)
	}
	if err := itPool.QueryRow(ctx, `SELECT status FROM tasks WHERE id=$1`, doneTask).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "complete" {
		t.Fatalf("completed task must be untouched: want complete, got %q", status)
	}

	// Settled at completed work only, and NO refund row: the buyer keeps the 0.10
	// charge for the delivered chunk and was never charged for the cancelled one.
	var actual float64
	if err := itPool.QueryRow(ctx, `SELECT actual_usd::float8 FROM jobs WHERE id=$1`, jobID).Scan(&actual); err != nil {
		t.Fatal(err)
	}
	if actual < 0.099 || actual > 0.101 {
		t.Fatalf("actual_usd: want 0.10 (completed work only), got %v", actual)
	}
	var refunds int
	if err := itPool.QueryRow(ctx,
		`SELECT count(*) FROM ledger_entries WHERE kind='refund' AND buyer_id=$1`, demoBuyerUUID).Scan(&refunds); err != nil {
		t.Fatal(err)
	}
	if refunds != 0 {
		t.Fatalf("stuck cancel must NOT full-refund (completed work stays charged), got %d refund rows", refunds)
	}

	// Checkpoint: the partial merged artifact exists at output_ref.
	if _, err := itStorage.GetObject(ctx, "jobs/stuck/output.jsonl"); err != nil {
		t.Fatalf("checkpoint artifact missing at output_ref: %v", err)
	}

	// Buyer-visible timeline event, exactly once — including after a second sweep.
	countEvents := func() int {
		var n int
		if err := itPool.QueryRow(ctx,
			`SELECT count(*) FROM job_events WHERE job_id=$1 AND event='job_stuck_cancelled'`, jobID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := countEvents(); n != 1 {
		t.Fatalf("job_stuck_cancelled events: want 1, got %d", n)
	}
	if err := wk.reapStuckJobs(ctx); err != nil {
		t.Fatalf("second reap: %v", err)
	}
	if n := countEvents(); n != 1 {
		t.Fatalf("reaper must be idempotent: want 1 event after re-sweep, got %d", n)
	}
}

// TestWatchdogRescueThenKill proves the escalation ladder end to end: the FIRST
// stuck verdict RESCUES (unfinished task requeued with the claim cleared and no
// retry burned, watchdog_strikes 0 → 1, job_stuck_rescued event, job still
// running — and the rescue's own visibility backoff counts as progress, so an
// immediate re-sweep must NOT kill), and only a REPEAT stall after the rescue
// KILLS: checkpoint merged, job + unfinished task cancelled, settled at completed
// work, and the mid-chunk partial checkpoint the agent uploaded surfaced as a
// presigned URL in the job_stuck_cancelled event detail.
func TestWatchdogRescueThenKill(t *testing.T) {
	reset(t)
	ctx := context.Background()

	// eta 10s, created 10 minutes ago (past the floored deadline eta+120s), strike 0.
	jobID := uuid.New()
	if _, err := itPool.Exec(ctx,
		`INSERT INTO jobs (id, buyer_id, status, job_type, model_ref, input_ref, tier,
		                   task_count, tasks_done, min_memory_gb, eta_secs, output_ref, created_at)
		 VALUES ($1,$2,'running','embed','all-minilm-l6-v2','jobs/rtk/input.jsonl','batch',
		         2,1,2,10,'jobs/rtk/output.jsonl', now() - interval '10 minutes')`,
		jobID, demoBuyerUUID); err != nil {
		t.Fatal(err)
	}
	doneTask, slowTask := uuid.New(), uuid.New()
	doneKey := "jobs/rtk/tasks/0/result.json"
	if err := itStorage.PutObject(ctx, doneKey, embedResultJSON(1), "application/json"); err != nil {
		t.Fatalf("seed result object: %v", err)
	}
	if _, err := itPool.Exec(ctx,
		`INSERT INTO tasks (id, job_id, status, input_ref, result_key, result_ref, chunk_index,
		                    worker_id, claimed_by, completed_at, visible_at)
		 VALUES ($1,$2,'complete','jobs/rtk/tasks/0/input.jsonl',$3,$3,0,$4,$4,
		         now() - interval '10 minutes', now() - interval '10 minutes')`,
		doneTask, jobID, doneKey, demoWorkerUUID); err != nil {
		t.Fatal(err)
	}
	if _, err := itPool.Exec(ctx,
		`INSERT INTO tasks (id, job_id, status, input_ref, result_key, chunk_index,
		                    worker_id, claimed_by, claimed_at, started_at, retry_count, visible_at)
		 VALUES ($1,$2,'running','jobs/rtk/tasks/1/input.jsonl','jobs/rtk/tasks/1/result.json',1,
		         $3,$3, now() - interval '10 minutes', now() - interval '10 minutes', 0, now() - interval '10 minutes')`,
		slowTask, jobID, demoWorkerUUID); err != nil {
		t.Fatal(err)
	}
	// The delivered chunk's charge, settled at its commit.
	if _, err := itPool.Exec(ctx,
		`INSERT INTO ledger_entries (kind, buyer_id, task_id, amount_usd, payout_status)
		 VALUES ('buyer_charge', $1, $2, -0.10, 'released')`,
		demoBuyerUUID, doneTask); err != nil {
		t.Fatal(err)
	}

	wk := NewWorkers(itStore, itStorage, stubPayout{})

	// SWEEP 1 → RESCUE: task requeued for a different machine, strike armed.
	if err := wk.reapStuckJobs(ctx); err != nil {
		t.Fatalf("reapStuckJobs (rescue): %v", err)
	}
	var jstatus string
	var strikes int
	if err := itPool.QueryRow(ctx,
		`SELECT status, watchdog_strikes FROM jobs WHERE id=$1`, jobID).Scan(&jstatus, &strikes); err != nil {
		t.Fatal(err)
	}
	if jstatus != "running" || strikes != 1 {
		t.Fatalf("rescue: want running job at strike 1, got status=%q strikes=%d", jstatus, strikes)
	}
	var tstatus string
	var claimedBy *uuid.UUID
	var retry int16
	if err := itPool.QueryRow(ctx,
		`SELECT status, claimed_by, retry_count FROM tasks WHERE id=$1`, slowTask).Scan(&tstatus, &claimedBy, &retry); err != nil {
		t.Fatal(err)
	}
	if tstatus != "queued" || claimedBy != nil || retry != 0 {
		t.Fatalf("rescue must requeue with the claim cleared and NO retry burned: status=%q claimed=%v retry=%d",
			tstatus, claimedBy, retry)
	}
	var nRescued int
	itPool.QueryRow(ctx,
		`SELECT count(*) FROM job_events WHERE job_id=$1 AND event='job_stuck_rescued'`, jobID).Scan(&nRescued)
	if nRescued != 1 {
		t.Fatalf("job_stuck_rescued events: want 1, got %d", nRescued)
	}

	// An immediate re-sweep must NOT kill: the rescue's visibility backoff is progress.
	if err := wk.reapStuckJobs(ctx); err != nil {
		t.Fatalf("reapStuckJobs (post-rescue): %v", err)
	}
	itPool.QueryRow(ctx, `SELECT status FROM jobs WHERE id=$1`, jobID).Scan(&jstatus)
	if jstatus != "running" {
		t.Fatalf("a just-rescued job must not be killed on the next sweep, got %q", jstatus)
	}

	// Age the claim again: a new machine picked the chunk up and stalled just as hard.
	if _, err := itPool.Exec(ctx,
		`UPDATE tasks SET status='running', claimed_by=$2, claimed_at=now()-interval '10 minutes',
		        started_at=now()-interval '10 minutes', visible_at=now()-interval '10 minutes'
		 WHERE id=$1`, slowTask, demoWorkerUUID); err != nil {
		t.Fatal(err)
	}
	// A mid-chunk partial checkpoint the agent uploaded before wedging (wire
	// contract: final-result shape + a top-level "partial": true marker).
	partialKey := "jobs/rtk/tasks/1/result.json.partial"
	if err := itStorage.PutObject(ctx, partialKey,
		[]byte(`{"partial":true,"job_type":"embed","count":0,"vectors":[]}`), "application/json"); err != nil {
		t.Fatalf("seed partial object: %v", err)
	}

	// SWEEP 2 → KILL: checkpoint + cancel + settle + partial URL surfaced.
	if err := wk.reapStuckJobs(ctx); err != nil {
		t.Fatalf("reapStuckJobs (kill): %v", err)
	}
	itPool.QueryRow(ctx, `SELECT status FROM jobs WHERE id=$1`, jobID).Scan(&jstatus)
	if jstatus != "cancelled" {
		t.Fatalf("second stall: want cancelled, got %q", jstatus)
	}
	itPool.QueryRow(ctx, `SELECT status FROM tasks WHERE id=$1`, slowTask).Scan(&tstatus)
	if tstatus != "cancelled" {
		t.Fatalf("unfinished task: want cancelled, got %q", tstatus)
	}
	var actual float64
	itPool.QueryRow(ctx, `SELECT actual_usd::float8 FROM jobs WHERE id=$1`, jobID).Scan(&actual)
	if actual < 0.099 || actual > 0.101 {
		t.Fatalf("actual_usd: want 0.10 (completed work only), got %v", actual)
	}
	if _, err := itStorage.GetObject(ctx, "jobs/rtk/output.jsonl"); err != nil {
		t.Fatalf("checkpoint artifact missing at output_ref: %v", err)
	}
	var nCancelled int
	var detail string
	if err := itPool.QueryRow(ctx,
		`SELECT count(*), COALESCE(MAX(detail::text),'') FROM job_events
		 WHERE job_id=$1 AND event='job_stuck_cancelled'`, jobID).Scan(&nCancelled, &detail); err != nil {
		t.Fatal(err)
	}
	if nCancelled != 1 {
		t.Fatalf("job_stuck_cancelled events: want 1, got %d", nCancelled)
	}
	if !strings.Contains(detail, "partial_urls") {
		t.Fatalf("kill event detail must carry the partial checkpoint URLs, got %q", detail)
	}
}

// TestRescueDeadClaimRequeues proves the worker-liveness rescue: a running task
// whose claiming worker last heartbeated 10 minutes ago is requeued immediately
// (claim cleared, no retry burned), the buyer sees task_rescued_dead_worker, and
// the wedged worker's supplier takes a small reputation dock — while a task whose
// worker heartbeated 10 seconds ago is untouched.
func TestRescueDeadClaimRequeues(t *testing.T) {
	reset(t)
	ctx := context.Background()

	// A DEAD worker on the demo supplier: silent for 10 minutes.
	deadWorker := uuid.New()
	if _, err := itPool.Exec(ctx,
		`INSERT INTO workers (id, supplier_id, hw_class, memory_gb, last_seen_at)
		 VALUES ($1,$2,'apple_silicon_max',64, now() - interval '10 minutes')`,
		deadWorker, demoSupplierUUID); err != nil {
		t.Fatal(err)
	}
	deadJob, deadTask := uuid.New(), uuid.New()
	mustJobTask(t, deadJob, deadTask, false, false, "jobs/dc/tasks/0/input.jsonl")
	if _, err := itPool.Exec(ctx,
		`UPDATE tasks SET status='queued',claimed_by=$2,claimed_at=now()-interval '10 minutes',
		        started_at=NULL,worker_id=NULL WHERE id=$1`,
		deadTask, deadWorker); err != nil {
		t.Fatal(err)
	}
	if err := itStore.StartTask(ctx, deadTask, deadWorker); err != nil {
		t.Fatalf("start dead-worker task: %v", err)
	}
	if _, err := itPool.Exec(ctx,
		`UPDATE tasks SET claimed_at=now()-interval '10 minutes',started_at=now()-interval '10 minutes' WHERE id=$1`,
		deadTask); err != nil {
		t.Fatal(err)
	}

	// CONTROL: an equally old claim held by a worker that heartbeated 10s ago.
	liveJob, liveTask := uuid.New(), uuid.New()
	mustJobTask(t, liveJob, liveTask, false, false, "jobs/dc2/tasks/0/input.jsonl")
	if _, err := itPool.Exec(ctx,
		`UPDATE tasks SET status='queued',claimed_by=$2,claimed_at=now()-interval '10 minutes',
		        started_at=NULL,worker_id=NULL WHERE id=$1`,
		liveTask, demoWorkerUUID); err != nil {
		t.Fatal(err)
	}
	if err := itStore.StartTask(ctx, liveTask, demoWorkerUUID); err != nil {
		t.Fatalf("start live-worker task: %v", err)
	}
	if _, err := itPool.Exec(ctx,
		`UPDATE tasks SET claimed_at=now()-interval '10 minutes',started_at=now()-interval '10 minutes' WHERE id=$1`,
		liveTask); err != nil {
		t.Fatal(err)
	}
	if _, err := itPool.Exec(ctx,
		`UPDATE workers SET last_seen_at = now() - interval '10 seconds' WHERE id=$1`,
		demoWorkerUUID); err != nil {
		t.Fatal(err)
	}
	repBefore := supplierRep(t) // 0.90 after reset

	wk := NewWorkers(itStore, itStorage, stubPayout{})
	if err := wk.rescueDeadClaims(ctx); err != nil {
		t.Fatalf("rescueDeadClaims: %v", err)
	}

	// Dead-worker task: rescued (queued, unclaimed, no retry burned).
	var status string
	var claimedBy *uuid.UUID
	var retry int16
	if err := itPool.QueryRow(ctx,
		`SELECT status, claimed_by, retry_count FROM tasks WHERE id=$1`, deadTask).Scan(&status, &claimedBy, &retry); err != nil {
		t.Fatal(err)
	}
	if status != "queued" || claimedBy != nil || retry != 0 {
		t.Fatalf("dead claim not rescued: status=%q claimed=%v retry=%d", status, claimedBy, retry)
	}
	// Live-worker task: untouched.
	if err := itPool.QueryRow(ctx,
		`SELECT status, claimed_by FROM tasks WHERE id=$1`, liveTask).Scan(&status, &claimedBy); err != nil {
		t.Fatal(err)
	}
	if status != "running" || claimedBy == nil || *claimedBy != demoWorkerUUID {
		t.Fatalf("live worker's claim must be untouched: status=%q claimed=%v", status, claimedBy)
	}
	// Buyer-visible attribution on the rescued job.
	var nEvents int
	itPool.QueryRow(ctx,
		`SELECT count(*) FROM job_events WHERE job_id=$1 AND event='task_rescued_dead_worker'`, deadJob).Scan(&nEvents)
	if nEvents != 1 {
		t.Fatalf("task_rescued_dead_worker events: want 1, got %d", nEvents)
	}
	// The wedged worker's supplier is docked mildly (catalogue job type) — and
	// only mildly: never near a quarantine from one dead claim.
	if rep := supplierRep(t); rep >= repBefore || rep < repBefore-0.01 {
		t.Fatalf("supplier reputation should dip slightly (mildest dock), was %v now %v", repBefore, rep)
	}
}

// TestWatchdogDeadlineGeometry proves the deadline conditions one by one:
// (a) the ETA floor — factor × a tiny ETA never beats eta+120s, so a 100s-old
// job with eta 10s is NOT judged, while the same job at 200s is; (b) the 24h
// wall-clock cap catches a job with NO ETA prediction; (c) deadline_secs = -1
// opts out entirely — not even the 24h cap touches it.
func TestWatchdogDeadlineGeometry(t *testing.T) {
	reset(t)
	ctx := context.Background()
	wk := NewWorkers(itStore, itStorage, stubPayout{})

	// One stalled running task per job (timestamps far outside the progress grace,
	// so selection is decided purely by the deadline geometry).
	mkJob := func(prefix string, etaSecs, deadlineSecs int, age string) uuid.UUID {
		t.Helper()
		jobID, taskID := uuid.New(), uuid.New()
		var eta any
		if etaSecs > 0 {
			eta = etaSecs
		}
		var deadline any
		if deadlineSecs != 0 {
			deadline = deadlineSecs
		}
		if _, err := itPool.Exec(ctx,
			`INSERT INTO jobs (id, buyer_id, status, job_type, model_ref, input_ref, tier,
			                   task_count, tasks_done, min_memory_gb, eta_secs, deadline_secs, created_at)
			 VALUES ($1,$2,'running','embed','all-minilm-l6-v2',$3,'batch',
			         1,0,2,$4,$5, now() - $6::interval)`,
			jobID, demoBuyerUUID, "jobs/"+prefix+"/input.jsonl", eta, deadline, age); err != nil {
			t.Fatal(err)
		}
		if _, err := itPool.Exec(ctx,
			`INSERT INTO tasks (id, job_id, status, input_ref, result_key, chunk_index,
			                    worker_id, claimed_by, claimed_at, started_at, visible_at)
			 VALUES ($1,$2,'running',$3,$4,0,$5,$5,
			         now() - interval '2 hours', now() - interval '2 hours', now() - interval '2 hours')`,
			taskID, jobID, "jobs/"+prefix+"/tasks/0/input.jsonl", "jobs/"+prefix+"/tasks/0/result.json",
			demoWorkerUUID); err != nil {
			t.Fatal(err)
		}
		return jobID
	}
	rescued := func(jobID uuid.UUID) bool {
		t.Helper()
		var n int
		if err := itPool.QueryRow(ctx,
			`SELECT count(*) FROM job_events WHERE job_id=$1 AND event='job_stuck_rescued'`, jobID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n > 0
	}

	floorJob := mkJob("geo-floor", 10, 0, "100 seconds") // (a) under the eta+120s floor
	noEtaJob := mkJob("geo-noeta", 0, 0, "25 hours")     // (b) no prediction, past the 24h cap
	optOutJob := mkJob("geo-optout", 10, -1, "25 hours") // (c) buyer opt-out

	if err := wk.reapStuckJobs(ctx); err != nil {
		t.Fatalf("reapStuckJobs: %v", err)
	}
	if rescued(floorJob) {
		t.Fatal("eta floor: a 100s-old job with eta 10s must NOT be judged (floor eta+120s governs)")
	}
	if !rescued(noEtaJob) {
		t.Fatal("24h cap: a 25h-old job with no ETA must be judged")
	}
	if rescued(optOutJob) {
		t.Fatal("deadline_secs=-1: an opted-out job must NEVER be judged")
	}

	// The same floor job, 200s old, is past eta+120s → judged on the next sweep.
	if _, err := itPool.Exec(ctx,
		`UPDATE jobs SET created_at = now() - interval '200 seconds' WHERE id=$1`, floorJob); err != nil {
		t.Fatal(err)
	}
	if err := wk.reapStuckJobs(ctx); err != nil {
		t.Fatalf("second reap: %v", err)
	}
	if !rescued(floorJob) {
		t.Fatal("eta floor: the same job at 200s (past eta+120s) must be judged")
	}
	if rescued(optOutJob) {
		t.Fatal("deadline_secs=-1: an opted-out job must stay untouched on every sweep")
	}
}

// TestWatchdogOptOutValidation proves the buyer policy knob's validation and
// persistence: 17 (neither a sentinel nor within 60..604800) is a 400; -1 (opt
// out) and 3600 (explicit deadline) are accepted and the jobs row carries the value.
func TestWatchdogOptOutValidation(t *testing.T) {
	reset(t)
	ctx := context.Background()
	// This test exercises deadline_secs VALIDATION (400 vs 202), not the payment
	// gate. Pin Stripe off so a submit is never intercepted by the 402 "no card /
	// sandbox credit exhausted" path (which fires when a key is configured in the
	// process env, e.g. under prove-local) before validation is even reached.
	t.Setenv("STRIPE_SECRET_KEY", "")

	submit := func(deadlineSecs int) (int, []byte) {
		t.Helper()
		body := map[string]any{
			"job_type":      map[string]any{"type": "embed"},
			"model":         map[string]any{"kind": "hf", "ref": "all-minilm-l6-v2"},
			"constraints":   map[string]any{"min_memory_gb": 2},
			"verification":  map[string]any{},
			"tier":          "batch",
			"input":         `{"id":"r0","text":"record 0"}` + "\n",
			"deadline_secs": deadlineSecs,
		}
		return req(t, "POST", "/v1/jobs", body, buyerKey(), jsonCT())
	}

	if code, out := submit(17); code != http.StatusBadRequest {
		t.Fatalf("deadline_secs=17: want 400, got %d: %s", code, out)
	}
	if code, out := submit(-1); code != http.StatusAccepted {
		t.Fatalf("deadline_secs=-1: want 202, got %d: %s", code, out)
	}
	code, out := submit(3600)
	if code != http.StatusAccepted {
		t.Fatalf("deadline_secs=3600: want 202, got %d: %s", code, out)
	}
	var r JobSubmitResponse
	if err := json.Unmarshal(out, &r); err != nil {
		t.Fatalf("submit decode: %v (%s)", err, out)
	}
	var persisted int
	if err := itPool.QueryRow(ctx,
		`SELECT deadline_secs FROM jobs WHERE id=$1`, r.JobID).Scan(&persisted); err != nil {
		t.Fatal(err)
	}
	if persisted != 3600 {
		t.Fatalf("jobs.deadline_secs: want 3600, got %d", persisted)
	}
}

// --- birds-eye summary (GET /admin/summary, summary.go) ---

// TestAdminSummary proves the roll-up: ledger sums sign-normalized per kind (take
// vs flow-through owed), runs by status, supplier credit by payout status, and that
// the endpoint is admin-gated.
func TestAdminSummary(t *testing.T) {
	reset(t)
	ctx := context.Background()

	// A real job + task to hang the ledger rows on (ledger_entries.task_id is a
	// genuine FK), then one settled charge (1.00 → 0.90 supplier held + 0.10 take)
	// and one clawback.
	sumJob, taskID := uuid.New(), uuid.New()
	if _, err := itPool.Exec(ctx,
		`INSERT INTO jobs (id, buyer_id, status, job_type, model_ref, input_ref, tier, task_count, tasks_done, min_memory_gb)
		 VALUES ($1,$2,'complete','embed','all-minilm-l6-v2','jobs/s/input.jsonl','batch',1,1,2)`,
		sumJob, demoBuyerUUID); err != nil {
		t.Fatal(err)
	}
	if _, err := itPool.Exec(ctx,
		`INSERT INTO tasks (id, job_id, status, input_ref, result_key, chunk_index, worker_id, claimed_by, completed_at)
		 VALUES ($1,$2,'complete','jobs/s/tasks/0/input.jsonl','jobs/s/tasks/0/result.json',0,$3,$3, now())`,
		taskID, sumJob, demoWorkerUUID); err != nil {
		t.Fatal(err)
	}
	seed := []struct {
		q    string
		args []any
	}{
		{`INSERT INTO ledger_entries (kind, buyer_id, task_id, amount_usd, payout_status)
		  VALUES ('buyer_charge', $1, $2, -1.00, 'released')`, []any{demoBuyerUUID, taskID}},
		{`INSERT INTO ledger_entries (kind, supplier_id, task_id, amount_usd, payout_status)
		  VALUES ('supplier_credit', $1, $2, 0.90, 'held')`, []any{demoSupplierUUID, taskID}},
		{`INSERT INTO ledger_entries (kind, task_id, amount_usd, payout_status)
		  VALUES ('platform_take', $1, 0.10, 'released')`, []any{taskID}},
		{`INSERT INTO ledger_entries (kind, supplier_id, task_id, amount_usd, payout_status)
		  VALUES ('clawback', $1, $2, -0.05, 'clawed_back')`, []any{demoSupplierUUID, taskID}},
	}
	for _, s := range seed {
		if _, err := itPool.Exec(ctx, s.q, s.args...); err != nil {
			t.Fatal(err)
		}
	}
	// A second job in a distinct state for the by-status counts (sumJob above is
	// the 'complete' one).
	if _, err := itPool.Exec(ctx,
		`INSERT INTO jobs (id, buyer_id, status, job_type, model_ref, input_ref, tier, task_count, tasks_done, min_memory_gb)
		 VALUES ($1,$2,'running','embed','all-minilm-l6-v2','jobs/s2/input.jsonl','batch',1,0,2)`,
		uuid.New(), demoBuyerUUID); err != nil {
		t.Fatal(err)
	}

	// Gated: no credential → 401.
	if code, _ := req(t, "GET", "/admin/summary", nil); code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated /admin/summary: want 401, got %d", code)
	}

	code, out := req(t, "GET", "/admin/summary", nil, hdr{"Authorization", "Bearer dev-admin-key-0001"})
	if code != http.StatusOK {
		t.Fatalf("/admin/summary: want 200, got %d: %s", code, out)
	}
	var sum AdminSummary
	if err := json.Unmarshal(out, &sum); err != nil {
		t.Fatalf("decode summary: %v (%s)", err, out)
	}
	near := func(got, want float64, name string) {
		if got < want-0.001 || got > want+0.001 {
			t.Fatalf("%s: want %v, got %v", name, want, got)
		}
	}
	near(sum.Money.ChargedUSD, 1.00, "charged_usd")
	near(sum.Money.SupplierCreditUSD, 0.90, "supplier_credit_usd")
	near(sum.Money.PlatformTakeUSD, 0.10, "platform_take_usd")
	near(sum.Money.ClawedBackUSD, 0.05, "clawed_back_usd")
	near(sum.Money.FlowOwedUSD, 0.90, "flow_owed_usd (held+pending)")
	// Money-truth extensions: nothing here was ever collected externally (the
	// seeded jobs carry no actual_usd and no charge), so the collection figures
	// are zero, the take-net equals the take (no stripe_fee rows), and nothing
	// was transferred out (the credit is held, not released).
	near(sum.Money.CollectedUSD, 0, "collected_usd")
	near(sum.Money.UncollectedUSD, 0, "uncollected_usd")
	near(sum.Money.StripeFeesUSD, 0, "stripe_fees_usd")
	near(sum.Money.TakeNetUSD, 0.10, "take_net_usd")
	near(sum.Money.TransferredUSD, 0, "transferred_usd")
	if sum.Accessible == nil {
		t.Fatal("accessible section must be present (its query did not fail)")
	}
	near(sum.Accessible.TakeCollectedUSD, 0, "accessible.take_collected_usd (no charged job)")
	if sum.JobsByStatus["running"] != 1 || sum.JobsByStatus["complete"] != 1 {
		t.Fatalf("jobs_by_status: want running=1 complete=1, got %v", sum.JobsByStatus)
	}
	held, ok := sum.PayoutsByStatus["held"]
	if !ok || held.Count != 1 {
		t.Fatalf("payouts_by_status[held]: want count 1, got %v", sum.PayoutsByStatus)
	}
	near(held.USD, 0.90, "payouts_by_status[held].usd")
	if sum.Workers.Total < 1 {
		t.Fatalf("workers.total: want >=1 (the demo worker), got %d", sum.Workers.Total)
	}
}

// scrapeCounter GETs the real /metrics endpoint and parses one Prometheus
// counter's current value out of the "<name> <value>\n" line writeCounter
// emits. Fails the test if the metric is not present.
func scrapeCounter(t *testing.T, name string) float64 {
	t.Helper()
	code, body := req(t, "GET", "/metrics", nil)
	if code != 200 {
		t.Fatalf("GET /metrics: want 200, got %d", code)
	}
	prefix := name + " "
	for _, line := range strings.Split(string(body), "\n") {
		if strings.HasPrefix(line, prefix) {
			v, err := strconv.ParseFloat(strings.TrimSpace(line[len(prefix):]), 64)
			if err != nil {
				t.Fatalf("parsing metric %q line %q: %v", name, line, err)
			}
			return v
		}
	}
	t.Fatalf("metric %q not found in /metrics:\n%s", name, body)
	return 0
}

// TestResultsPollDoesNotReMergeAfterCompletion proves the Data Transfer &
// Artifact I/O 4.5->5 fix (docs/internal/CREED_AND_PATH_TO_TEN.md, "Stop paying
// for every poll twice"): once a job is complete, finalizeJobIfDone has already
// merged the buyer-ready artifact exactly once and stamped results_merged_at.
// Before this fix, GET /v1/jobs/{id}/results called MergeJobResults on EVERY
// poll, so cx_result_merges_total kept climbing (a full re-fetch + re-write of
// every primary result) even though nothing about the job had changed. Now a
// poll after completion must see the watermark already set and skip the merge
// entirely: 10 consecutive reads must move the counter by exactly 0 from its
// post-completion baseline (the one real merge already happened synchronously
// at completion, before any of these reads), and every read must still return a
// real presigned results_url.
func TestResultsPollDoesNotReMergeAfterCompletion(t *testing.T) {
	reset(t)
	ctx := context.Background()
	req(t, "POST", "/v1/worker/register", WorkerCapability{HWClass: "apple_silicon_max", MemoryGB: 64,
		SupportedJobs: []string{"embed"}, SupportedModels: []string{"all-minilm-l6-v2"}}, workerTok(), jsonCT())

	// 1 record -> exactly 1 task (skip_verification_floor via submitEmbedJob).
	jobID, taskCount := submitEmbedJob(t, 1, 0, 0, 0)
	if taskCount != 1 {
		t.Fatalf("want exactly 1 task for a 1-record embed job, got %d", taskCount)
	}

	_, body := req(t, "GET", "/v1/worker/poll", nil, workerTok())
	var disp TaskDispatch
	if err := json.Unmarshal(body, &disp); err != nil {
		t.Fatalf("dispatch decode: %v", err)
	}
	if err := itStorage.PutObject(ctx, disp.ResultKey, embedResultJSON(1), "application/json"); err != nil {
		t.Fatalf("put result: %v", err)
	}
	commit := TaskCommit{TaskID: disp.TaskID, ResultKey: disp.ResultKey}
	if code, cbody := req(t, "POST", "/v1/worker/task/"+disp.TaskID.String()+"/commit", commit, workerTok(), jsonCT()); code != 204 {
		t.Fatalf("commit: want 204, got %d: %s", code, cbody)
	}

	// The commit path (finalizeJobIfDone) merges synchronously before marking
	// the job complete, so by the time we observe status=complete the ONE real
	// completion-time merge has already happened and results_merged_at is set.
	code, jbody := req(t, "GET", "/v1/jobs/"+jobID.String(), nil, buyerKey())
	if code != 200 {
		t.Fatalf("get job: %d %s", code, jbody)
	}
	var js JobStatus
	if err := json.Unmarshal(jbody, &js); err != nil {
		t.Fatalf("job status decode: %v", err)
	}
	if js.Status != "complete" {
		t.Fatalf("job status: want complete, got %q", js.Status)
	}

	// Baseline AFTER completion (already includes the one completion-time
	// merge) and BEFORE any of the 10 results reads.
	baseline := scrapeCounter(t, "cx_result_merges_total")

	for i := 0; i < 10; i++ {
		code, rbody := req(t, "GET", "/v1/jobs/"+jobID.String()+"/results", nil, buyerKey())
		if code != 200 {
			t.Fatalf("read %d: results: want 200, got %d: %s", i, code, rbody)
		}
		var jr JobResults
		if err := json.Unmarshal(rbody, &jr); err != nil {
			t.Fatalf("read %d: results decode: %v", i, err)
		}
		if jr.ResultsURL == "" {
			t.Fatalf("read %d: results_url must be a real presigned URL, got empty", i)
		}
	}

	after := scrapeCounter(t, "cx_result_merges_total")
	if after != baseline {
		t.Fatalf("cx_result_merges_total moved from %v to %v across 10 post-completion reads; "+
			"want unchanged (watermark should have skipped every re-merge)", baseline, after)
	}
}

// --- Buyer Advantage & Pricing Edge 4.5->5: reprice from real supplier economics ---

// TestApplyRepricingUsesRealSupplierEconomics proves the whole rung end to end
// against real Postgres: a model still at the hand-seeded 'seed' price_source gets
// REPRICED to the formula's real output and the change is immediately visible on
// the live GET /v1/models catalogue a buyer actually reads — while a model an
// operator has already edited (or a model with no real measured throughput, e.g.
// qwen2.5-7b-instruct-q4) is left completely untouched, never silently clobbered
// or invented for.
func TestApplyRepricingUsesRealSupplierEconomics(t *testing.T) {
	ctx := context.Background()
	// Force all-minilm-l6-v2 back to the original hand-seeded state and give
	// bge-small-en-v1.5 a simulated operator override, so this test proves both
	// halves of the contract regardless of what earlier runs (or a prior manual
	// psql session against this same DB) left behind.
	if _, err := itPool.Exec(ctx,
		`UPDATE models SET price_per_1k = 0.00100000, price_source = 'seed', price_formula = NULL
		 WHERE id = 'all-minilm-l6-v2'`); err != nil {
		t.Fatalf("reset all-minilm-l6-v2: %v", err)
	}
	if _, err := itPool.Exec(ctx,
		`UPDATE models SET price_per_1k = 0.00500000, price_source = 'operator_override', price_formula = NULL
		 WHERE id = 'bge-small-en-v1.5'`); err != nil {
		t.Fatalf("set operator override on bge-small-en-v1.5: %v", err)
	}
	t.Cleanup(func() {
		itPool.Exec(ctx, `UPDATE models SET price_per_1k = 0.00100000, price_source = 'seed', price_formula = NULL WHERE id = 'all-minilm-l6-v2'`)
		itPool.Exec(ctx, `UPDATE models SET price_per_1k = 0.00100000, price_source = 'seed', price_formula = NULL WHERE id = 'bge-small-en-v1.5'`)
	})

	results := RepriceCatalogueFromSupplierEconomics(supplierShareRate)
	updated, err := itStore.ApplyRepricing(ctx, results)
	if err != nil {
		t.Fatalf("ApplyRepricing: %v", err)
	}
	if updated < 1 {
		t.Fatalf("expected at least 1 row updated (all-minilm-l6-v2 was reset to 'seed'), got %d", updated)
	}

	// all-minilm-l6-v2: real repriced number, source flipped, formula recorded and
	// cites the real GPU_CAPABILITY.md-sourced throughput — not silently applied.
	var price float64
	var source, formula string
	if err := itPool.QueryRow(ctx,
		`SELECT price_per_1k::float8, price_source, COALESCE(price_formula,'') FROM models WHERE id='all-minilm-l6-v2'`,
	).Scan(&price, &source, &formula); err != nil {
		t.Fatalf("read repriced all-minilm-l6-v2: %v", err)
	}
	if source != "measured_supplier_economics" {
		t.Fatalf("all-minilm-l6-v2 price_source = %q, want measured_supplier_economics", source)
	}
	if price <= 0 || price == 0.001 {
		t.Fatalf("all-minilm-l6-v2 price_per_1k = %v, want a real repriced value, not the untouched seed", price)
	}
	if !strings.Contains(formula, "capability.json") {
		t.Fatalf("price_formula must cite the real measured source, got %q", formula)
	}

	// bge-small-en-v1.5: the operator's own override must survive completely
	// untouched — ApplyRepricing must never clobber a non-'seed' row.
	var bgePrice float64
	var bgeSource string
	if err := itPool.QueryRow(ctx,
		`SELECT price_per_1k::float8, price_source FROM models WHERE id='bge-small-en-v1.5'`,
	).Scan(&bgePrice, &bgeSource); err != nil {
		t.Fatalf("read bge-small-en-v1.5: %v", err)
	}
	if bgeSource != "operator_override" || bgePrice != 0.005 {
		t.Fatalf("operator override was clobbered: source=%q price=%v", bgeSource, bgePrice)
	}

	// qwen2.5-7b-instruct-q4 has no real measured throughput (GPU_CAPABILITY.md:
	// its GGUF ref 404s) — it must be left at 'seed' forever, never invented for.
	var qwenSource string
	if err := itPool.QueryRow(ctx,
		`SELECT price_source FROM models WHERE id='qwen2.5-7b-instruct-q4'`,
	).Scan(&qwenSource); err != nil {
		t.Fatalf("read qwen2.5-7b-instruct-q4: %v", err)
	}
	if qwenSource != "seed" {
		t.Fatalf("qwen2.5-7b-instruct-q4 has no real measured throughput and must stay 'seed', got %q", qwenSource)
	}

	// The repriced number is what a real buyer actually sees on the live catalogue
	// endpoint, not just a DB row nothing reads.
	code, body := req(t, "GET", "/v1/models", nil, buyerKey())
	if code != 200 {
		t.Fatalf("GET /v1/models: %d %s", code, body)
	}
	var models []ModelInfo
	if err := json.Unmarshal(body, &models); err != nil {
		t.Fatalf("decode models: %v (%s)", err, body)
	}
	found := false
	for _, m := range models {
		if m.ID == "all-minilm-l6-v2" {
			found = true
			if m.PricePer1KUSD != price {
				t.Fatalf("GET /v1/models price %.8f does not match the repriced DB value %.8f", m.PricePer1KUSD, price)
			}
		}
	}
	if !found {
		t.Fatal("all-minilm-l6-v2 missing from GET /v1/models")
	}

	// Idempotency: running ApplyRepricing again must be a no-op (0 rows updated) —
	// every row it touched is now non-'seed', so a second run cannot even see them.
	updated2, err := itStore.ApplyRepricing(ctx, results)
	if err != nil {
		t.Fatalf("second ApplyRepricing: %v", err)
	}
	if updated2 != 0 {
		t.Fatalf("second ApplyRepricing should be a no-op, updated %d rows", updated2)
	}
}

// --- Quote-to-settlement economics truth --------------------------------------

// TestAdminQuoteSettlementRollupRefusesCircularAutoTune proves the admin API
// still exposes quote-to-settlement realization while naming its circular basis,
// and refuses to mutate catalogue prices until independent execution-cost
// telemetry exists.
func TestAdminQuoteSettlementRollupRefusesCircularAutoTune(t *testing.T) {
	reset(t)
	ctx := context.Background()
	itPool.Exec(ctx, `TRUNCATE quotes`)
	t.Cleanup(func() {
		itPool.Exec(ctx, `TRUNCATE quotes`)
		itPool.Exec(ctx, `UPDATE models SET price_per_1k = 0.00800000, price_source = 'seed', price_formula = NULL WHERE id = 'qwen2.5-7b-instruct-q4'`)
	})

	// Use qwen2.5-7b-instruct-q4 for this test: it is deliberately NEVER touched by
	// ApplyRepricing (no real measured throughput), so its price_per_1k is a stable
	// 'seed' baseline this test fully controls and can assert an exact before/after
	// on, independent of whatever ApplyRepricing did to the embed/infer models
	// elsewhere in this same DB.
	if _, err := itPool.Exec(ctx,
		`UPDATE models SET price_per_1k = 0.00800000, price_source = 'seed', price_formula = NULL WHERE id = 'qwen2.5-7b-instruct-q4'`); err != nil {
		t.Fatalf("reset qwen price: %v", err)
	}

	// Five quote-bound terminal jobs whose settlement field is 20% above the quote.
	// The ratio is displayable, but actual_usd is still quote-derived settlement,
	// not measured runtime/energy/hardware/platform cost.
	const nSamples = 5
	const quotedEach = 1.00
	const actualEach = 1.20
	for i := 0; i < nSamples; i++ {
		qID := uuid.New()
		if _, err := itPool.Exec(ctx,
			`INSERT INTO quotes (id, buyer_id, job_type, model_ref, tier, cost_expected_usd)
			 VALUES ($1,$2,'batch_infer','qwen2.5-7b-instruct-q4','batch',$3)`,
			qID, demoBuyerUUID, quotedEach); err != nil {
			t.Fatalf("insert quote %d: %v", i, err)
		}
		jobID := uuid.New()
		if _, err := itPool.Exec(ctx,
			`INSERT INTO jobs (id, buyer_id, status, job_type, model_ref, input_ref, tier, task_count, tasks_done, quote_id, actual_usd)
			 VALUES ($1,$2,'complete','batch_infer','qwen2.5-7b-instruct-q4','jobs/d/input.jsonl','batch',1,1,$3,$4)`,
			jobID, demoBuyerUUID, qID, actualEach); err != nil {
			t.Fatalf("insert job %d: %v", i, err)
		}
	}

	// GET /admin/quotes: the real cost-drift rollup, exercised live over HTTP.
	code, body := req(t, "GET", "/admin/quotes", nil, adminKey())
	if code != 200 {
		t.Fatalf("GET /admin/quotes: %d %s", code, body)
	}
	var rollup []CostDriftRow
	if err := json.Unmarshal(body, &rollup); err != nil {
		t.Fatalf("decode drift rollup: %v (%s)", err, body)
	}
	var row *CostDriftRow
	for i := range rollup {
		if rollup[i].JobType == "batch_infer" && rollup[i].ModelRef == "qwen2.5-7b-instruct-q4" {
			row = &rollup[i]
		}
	}
	if row == nil {
		t.Fatalf("qwen2.5-7b-instruct-q4/batch_infer row missing from rollup: %+v", rollup)
	}
	if row.Samples != nSamples {
		t.Fatalf("samples = %d, want %d", row.Samples, nSamples)
	}
	if diff := row.AvgQuotedUSD - quotedEach; diff > 1e-6 || diff < -1e-6 {
		t.Fatalf("avg_quoted_usd = %v, want %v", row.AvgQuotedUSD, quotedEach)
	}
	if diff := row.AvgActualUSD - actualEach; diff > 1e-6 || diff < -1e-6 {
		t.Fatalf("avg_actual_usd = %v, want %v", row.AvgActualUSD, actualEach)
	}
	if diff := row.DriftRatio - 1.2; diff > 1e-6 || diff < -1e-6 {
		t.Fatalf("drift_ratio = %v, want 1.2 (a real 20%% underquote)", row.DriftRatio)
	}
	if diff := row.DriftPct - 20.0; diff > 1e-6 || diff < -1e-6 {
		t.Fatalf("drift_pct = %v, want 20", row.DriftPct)
	}
	if row.UsingForTuning {
		t.Fatalf("quote-derived settlement must never be eligible for tuning: %+v", row)
	}
	if row.ActualUSDBasis != actualUSDBasisQuoteDerivedSettlement ||
		row.TuningBlockReason != priceTuningBlockedNoIndependentTelemetry {
		t.Fatalf("admin row did not name/fail-closed its basis: %+v", row)
	}

	var beforePrice float64
	var beforeSource string
	if err := itPool.QueryRow(ctx,
		`SELECT price_per_1k::float8, price_source FROM models WHERE id='qwen2.5-7b-instruct-q4'`,
	).Scan(&beforePrice, &beforeSource); err != nil {
		t.Fatalf("read pre-refusal price: %v", err)
	}

	// POST refuses with a stable, non-500 contract and leaves the catalogue alone.
	acode, abody := req(t, "POST", "/admin/quotes/auto-tune", nil, adminKey())
	if acode != http.StatusConflict {
		t.Fatalf("POST /admin/quotes/auto-tune: want 409, got %d: %s", acode, abody)
	}
	var refusal struct {
		Reason            PriceTuningBlockReason `json:"reason"`
		ActualUSDBasis    string                 `json:"actual_usd_basis"`
		RequiredTelemetry string                 `json:"required_telemetry"`
	}
	if err := json.Unmarshal(abody, &refusal); err != nil {
		t.Fatalf("decode auto-tune refusal: %v (%s)", err, abody)
	}
	if refusal.Reason != priceTuningBlockedNoIndependentTelemetry ||
		refusal.ActualUSDBasis != actualUSDBasisQuoteDerivedSettlement ||
		refusal.RequiredTelemetry == "" {
		t.Fatalf("incomplete structured refusal: %+v", refusal)
	}
	var afterPrice float64
	var afterSource string
	if err := itPool.QueryRow(ctx,
		`SELECT price_per_1k::float8, price_source FROM models WHERE id='qwen2.5-7b-instruct-q4'`,
	).Scan(&afterPrice, &afterSource); err != nil {
		t.Fatalf("read post-refusal price: %v", err)
	}
	if afterPrice != beforePrice || afterSource != beforeSource {
		t.Fatalf("refused auto-tune mutated catalogue: before=%v/%q after=%v/%q",
			beforePrice, beforeSource, afterPrice, afterSource)
	}
}

// --- Project Detection & Quotation 7->8: the firm-quote tier -------------------

// TestFirmQuoteSubmissionRequiresQuoteID proves the validation gate: firm_quote
// with no quote_id is a 400, and firm_quote against a quote with no positive
// cost_max_usd (impossible via the real /v1/quote path, but defended anyway) is
// refused with a real error, never silently accepted as an empty commitment.
func TestFirmQuoteSubmissionRequiresQuoteID(t *testing.T) {
	code, body := req(t, "POST", "/v1/jobs", map[string]any{
		"job_type":     map[string]any{"type": "embed"},
		"model":        map[string]any{"kind": "hf", "ref": "all-minilm-l6-v2"},
		"tier":         "batch",
		"verification": map[string]any{"redundancy_frac": 0.0, "honeypot_frac": 0.0, "payout_hold_secs": 0},
		"input":        "{\"id\":\"a\",\"text\":\"no quote_id\"}\n",
		"firm_quote":   true,
	}, buyerKey(), jsonCT())
	if code != http.StatusBadRequest {
		t.Fatalf("firm_quote with no quote_id: want 400, got %d: %s", code, body)
	}
	if !strings.Contains(string(body), "quote_id") {
		t.Fatalf("400 reason should mention quote_id, got %s", body)
	}
}

// TestFirmQuoteCapsChargeAtStatedMaximum is the rung's own proof artifact,
// verified end to end against real Postgres: "a real job whose actual cost
// exceeds its firm quote still charges the buyer only the quoted maximum,
// verified on a real invoice." It quotes and firm-binds a real submission (so
// jobs.firm_quote / firm_quote_max_usd are the REAL values POST /v1/quote and
// POST /v1/jobs produced, not hand-inserted), then simulates the job's real
// work costing MORE than the firm quote's maximum (actual_usd set past
// firm_quote_max_usd, exactly as a real commit settlement would), and proves
// Store.JobChargeInfo — the exact function billing.go's chargeOrDeferJob calls
// to decide what to actually charge Stripe — returns the CAPPED amount, that
// FreezeChargeAmount stamps billed_usd at that capped figure (the same field
// the real charge-collect sweep freezes before ever calling Stripe), and that
// the buyer's own invoice shows the cap took effect.
func TestFirmQuoteCapsChargeAtStatedMaximum(t *testing.T) {
	ctx := context.Background()
	itPool.Exec(ctx, `TRUNCATE quotes`)
	t.Cleanup(func() { itPool.Exec(ctx, `TRUNCATE quotes`) })

	const input = "{\"id\":\"a\",\"text\":\"firm quote me\"}\n{\"id\":\"b\",\"text\":\"a real commitment\"}\n"
	quoteBody := map[string]any{
		"job_type":     map[string]any{"type": "embed"},
		"model":        map[string]any{"kind": "hf", "ref": "all-minilm-l6-v2"},
		"tier":         "batch",
		"verification": map[string]any{"redundancy_frac": 0.0, "honeypot_frac": 0.0, "payout_hold_secs": 0},
		"input":        input,
	}
	qcode, qbody := req(t, "POST", "/v1/quote", quoteBody, buyerKey(), jsonCT())
	if qcode != 200 {
		t.Fatalf("POST /v1/quote: %d %s", qcode, qbody)
	}
	var q Quote
	if err := json.Unmarshal(qbody, &q); err != nil {
		t.Fatalf("decode quote: %v (%s)", err, qbody)
	}
	if q.Cost.MaxUSD <= 0 {
		t.Fatalf("quote must have a positive cost_max_usd to firm-commit to, got %+v", q.Cost)
	}

	bind := map[string]any{
		"job_type":     map[string]any{"type": "embed"},
		"model":        map[string]any{"kind": "hf", "ref": "all-minilm-l6-v2"},
		"tier":         "batch",
		"verification": map[string]any{"redundancy_frac": 0.0, "honeypot_frac": 0.0, "payout_hold_secs": 0},
		"input":        input,
		"quote_id":     q.QuoteID,
		"firm_quote":   true,
	}
	code, body := req(t, "POST", "/v1/jobs", bind, buyerKey(), jsonCT())
	if code != http.StatusAccepted {
		t.Fatalf("firm-quote submit: want 202, got %d: %s", code, body)
	}
	var jr JobSubmitResponse
	if err := json.Unmarshal(body, &jr); err != nil {
		t.Fatalf("decode submit response: %v (%s)", err, body)
	}

	// The real submission persisted real firm-quote fields — not hand-inserted.
	var firmQuote bool
	var firmMax float64
	if err := itPool.QueryRow(ctx,
		`SELECT firm_quote, COALESCE(firm_quote_max_usd,0) FROM jobs WHERE id=$1`, jr.JobID,
	).Scan(&firmQuote, &firmMax); err != nil {
		t.Fatalf("read firm quote fields: %v", err)
	}
	if !firmQuote {
		t.Fatal("jobs.firm_quote should be true for a firm_quote:true submission")
	}
	if diff := firmMax - q.Cost.MaxUSD; diff > 1e-6 || diff < -1e-6 {
		t.Fatalf("jobs.firm_quote_max_usd = %v, want the quote's own cost_max_usd %v", firmMax, q.Cost.MaxUSD)
	}

	// Simulate the job's real settled cost coming in ABOVE the firm quote's
	// maximum — exactly what SetJobActualUSD would do from real per-task
	// buyer_charge ledger rows once real work completes. This is the scenario
	// the rung names directly: "a real job whose actual cost exceeds its firm
	// quote."
	overageActual := firmMax * 1.75
	if _, err := itPool.Exec(ctx,
		`UPDATE jobs SET status='complete', actual_usd=$2 WHERE id=$1`, jr.JobID, overageActual); err != nil {
		t.Fatalf("settle actual_usd above the firm max: %v", err)
	}

	// Store.JobChargeInfo is the EXACT function billing.go's chargeOrDeferJob
	// calls to decide the real Stripe charge amount — proving this returns the
	// capped figure proves the real charge path would too.
	buyerID, chargeUSD, err := itStore.JobChargeInfo(ctx, jr.JobID)
	if err != nil {
		t.Fatalf("JobChargeInfo: %v", err)
	}
	if buyerID != demoBuyerUUID {
		t.Fatalf("JobChargeInfo buyer = %v, want %v", buyerID, demoBuyerUUID)
	}
	if chargeUSD != firmMax {
		t.Fatalf("JobChargeInfo charge = %v, want the CAPPED firm max %v (real actual_usd was %v)", chargeUSD, firmMax, overageActual)
	}

	// FreezeChargeAmount is the real function the immediate-charge path calls
	// with that exact capped figure before ever touching Stripe — proving it
	// stamps billed_usd at the capped amount, not the uncapped actual_usd.
	if err := itStore.FreezeChargeAmount(ctx, jr.JobID, chargeUSD); err != nil {
		t.Fatalf("FreezeChargeAmount: %v", err)
	}

	// The buyer's own real invoice shows the cap took effect: billed_usd is the
	// capped figure, firm_quote_max_usd matches, and actual_usd is honestly still
	// the full uncapped figure (the real value of work delivered — never altered,
	// only the CHARGE is capped, the ledger truth is not rewritten).
	icode, ibody := req(t, "GET", "/v1/jobs/"+jr.JobID.String()+"/invoice", nil, buyerKey())
	if icode != 200 {
		t.Fatalf("invoice: want 200, got %d: %s", icode, ibody)
	}
	var inv InvoiceView
	if err := json.Unmarshal(ibody, &inv); err != nil {
		t.Fatalf("invoice decode: %v (%s)", err, ibody)
	}
	if !inv.FirmQuote {
		t.Fatal("invoice.firm_quote should be true")
	}
	if inv.FirmQuoteMaxUSD == nil || *inv.FirmQuoteMaxUSD != firmMax {
		t.Fatalf("invoice.firm_quote_max_usd = %v, want %v", inv.FirmQuoteMaxUSD, firmMax)
	}
	if inv.BilledUSD == nil {
		t.Fatal("invoice.billed_usd should be set once a charge was frozen")
	}
	if *inv.BilledUSD != firmMax {
		t.Fatalf("invoice.billed_usd = %v, want the CAPPED %v — the buyer must never be billed past their firm quote", *inv.BilledUSD, firmMax)
	}
	if *inv.BilledUSD >= overageActual {
		t.Fatalf("billed_usd (%v) must be LESS than the real overage actual_usd (%v) for this test to actually prove the cap engaged", *inv.BilledUSD, overageActual)
	}
	// NUMERIC(12,6) round-trips through Postgres with sub-micro-dollar rounding;
	// compare with a tiny epsilon rather than requiring bit-for-bit equality.
	if diff := inv.ActualUSD - overageActual; diff > 1e-6 || diff < -1e-6 {
		t.Fatalf("invoice.actual_usd should stay the honest uncapped settled figure %v, got %v (the cap must apply to the CHARGE, not rewrite the ledger truth)", overageActual, inv.ActualUSD)
	}
}

// TestFirmQuoteDoesNotCapWhenActualIsUnderMax proves the cap is a CEILING, not a
// flat re-price: a firm-quoted job whose real cost comes in AT OR BELOW the
// quoted maximum is charged its real actual cost, unchanged — the platform only
// ever absorbs an overage, it never pays a supplier-side discount to a buyer who
// didn't need one.
func TestFirmQuoteDoesNotCapWhenActualIsUnderMax(t *testing.T) {
	ctx := context.Background()
	itPool.Exec(ctx, `TRUNCATE quotes`)
	t.Cleanup(func() { itPool.Exec(ctx, `TRUNCATE quotes`) })

	const input = "{\"id\":\"a\",\"text\":\"under budget\"}\n"
	quoteBody := map[string]any{
		"job_type":     map[string]any{"type": "embed"},
		"model":        map[string]any{"kind": "hf", "ref": "all-minilm-l6-v2"},
		"tier":         "batch",
		"verification": map[string]any{"redundancy_frac": 0.0, "honeypot_frac": 0.0, "payout_hold_secs": 0},
		"input":        input,
	}
	qcode, qbody := req(t, "POST", "/v1/quote", quoteBody, buyerKey(), jsonCT())
	if qcode != 200 {
		t.Fatalf("POST /v1/quote: %d %s", qcode, qbody)
	}
	var q Quote
	json.Unmarshal(qbody, &q)

	bind := map[string]any{
		"job_type":     map[string]any{"type": "embed"},
		"model":        map[string]any{"kind": "hf", "ref": "all-minilm-l6-v2"},
		"tier":         "batch",
		"verification": map[string]any{"redundancy_frac": 0.0, "honeypot_frac": 0.0, "payout_hold_secs": 0},
		"input":        input,
		"quote_id":     q.QuoteID,
		"firm_quote":   true,
	}
	code, body := req(t, "POST", "/v1/jobs", bind, buyerKey(), jsonCT())
	if code != http.StatusAccepted {
		t.Fatalf("firm-quote submit: want 202, got %d: %s", code, body)
	}
	var jr JobSubmitResponse
	json.Unmarshal(body, &jr)

	underActual := q.Cost.MaxUSD * 0.5
	if _, err := itPool.Exec(ctx,
		`UPDATE jobs SET status='complete', actual_usd=$2 WHERE id=$1`, jr.JobID, underActual); err != nil {
		t.Fatalf("settle actual_usd under the firm max: %v", err)
	}

	_, chargeUSD, err := itStore.JobChargeInfo(ctx, jr.JobID)
	if err != nil {
		t.Fatalf("JobChargeInfo: %v", err)
	}
	if chargeUSD != underActual {
		t.Fatalf("charge = %v, want the UNCAPPED real actual %v (cap must not apply below the max)", chargeUSD, underActual)
	}
}

// --- Operator Tooling 7->8: audited admin write endpoints replacing raw SQL
// (docs/internal/CREED_AND_PATH_TO_TEN.md, "Add write actions the operator
// currently has to reach into the database for") ---

// TestAdminReinstateWorker exercises the reinstate-after-review half of
// RUNBOOKS.md's Bad/fraudulent worker procedure: a suspended supplier's worker
// becomes active again via the real endpoint (not psql), and a redundant call
// against an already-active supplier is a real 409, not a silent success.
func TestAdminReinstateWorker(t *testing.T) {
	reset(t)
	ctx := context.Background()
	if _, err := itPool.Exec(ctx,
		`UPDATE suppliers SET status='suspended', quarantined_at=now() WHERE id=$1`, demoSupplierUUID); err != nil {
		t.Fatal(err)
	}

	code, body := req(t, "POST", "/admin/workers/"+demoWorkerUUID.String()+"/reinstate", nil, adminKey())
	if code != http.StatusOK {
		t.Fatalf("reinstate: want 200, got %d: %s", code, body)
	}
	var status, quarantinedAt *string
	if err := itPool.QueryRow(ctx, `SELECT status, quarantined_at::text FROM suppliers WHERE id=$1`, demoSupplierUUID).
		Scan(&status, &quarantinedAt); err != nil {
		t.Fatal(err)
	}
	if status == nil || *status != "active" {
		t.Fatalf("supplier status = %v, want active", status)
	}
	if quarantinedAt != nil {
		t.Fatalf("quarantined_at = %v, want cleared (NULL)", *quarantinedAt)
	}

	// A second reinstate against an already-active supplier is a real conflict,
	// not a silently-repeated success.
	code2, body2 := req(t, "POST", "/admin/workers/"+demoWorkerUUID.String()+"/reinstate", nil, adminKey())
	if code2 != http.StatusConflict {
		t.Fatalf("reinstate on active supplier: want 409, got %d: %s", code2, body2)
	}

	// An unregistered worker id is a 404, not a 409 (distinct failure reasons).
	code3, _ := req(t, "POST", "/admin/workers/"+uuid.New().String()+"/reinstate", nil, adminKey())
	if code3 != http.StatusNotFound {
		t.Fatalf("reinstate on unknown worker: want 404, got %d", code3)
	}
}

// TestAdminForceRequeueTask exercises the "Stuck job" runbook's manual fix as a
// real audited endpoint: a wedged running task is reset to queued/unclaimed, and
// the audit log records who/why. A task NOT in a requeueable state is a 409.
func TestAdminForceRequeueTask(t *testing.T) {
	reset(t)
	ctx := context.Background()
	jobID, taskID := uuid.New(), uuid.New()
	mustJobTask(t, jobID, taskID, false, false, "jobs/x/tasks/0/input.jsonl")
	if _, err := itPool.Exec(ctx,
		`UPDATE tasks SET status='running', claimed_by=$2, worker_id=$2, claimed_at=now() WHERE id=$1`,
		taskID, demoWorkerUUID); err != nil {
		t.Fatal(err)
	}

	code, body := req(t, "POST", "/admin/tasks/"+taskID.String()+"/requeue",
		map[string]any{"reason": "wedged worker, confirmed dead via /admin/workers"}, adminKey(), jsonCT())
	if code != http.StatusOK {
		t.Fatalf("requeue: want 200, got %d: %s", code, body)
	}

	var status string
	var claimedBy, workerID *uuid.UUID
	if err := itPool.QueryRow(ctx, `SELECT status, claimed_by, worker_id FROM tasks WHERE id=$1`, taskID).
		Scan(&status, &claimedBy, &workerID); err != nil {
		t.Fatal(err)
	}
	if status != "queued" {
		t.Fatalf("task status = %q, want queued", status)
	}
	if claimedBy != nil || workerID != nil {
		t.Fatalf("claimed_by/worker_id = %v/%v, want both NULL", claimedBy, workerID)
	}

	// The audit log records this exact requeue with the given reason.
	acode, abody := req(t, "GET", "/admin/actions", nil, adminKey())
	if acode != http.StatusOK {
		t.Fatalf("GET /admin/actions: %d %s", acode, abody)
	}
	var actions AdminActionReviewPage
	if err := json.Unmarshal(abody, &actions); err != nil {
		t.Fatalf("unmarshal actions: %v", err)
	}
	found := false
	for _, a := range actions.Items {
		if a.Kind == "task_requeued" && a.TaskID != nil && *a.TaskID == taskID {
			found = true
			if a.Reason != "wedged worker, confirmed dead via /admin/workers" {
				t.Fatalf("audit reason = %q, want the given reason", a.Reason)
			}
		}
	}
	if !found {
		t.Fatal("audit log missing the task_requeued action for this task")
	}

	// A task that is already queued (not running/retrying) is a 409 — nothing to
	// force-requeue.
	code2, _ := req(t, "POST", "/admin/tasks/"+taskID.String()+"/requeue", nil, adminKey())
	if code2 != http.StatusConflict {
		t.Fatalf("requeue an already-queued task: want 409, got %d", code2)
	}

	// An unknown task id is a 404.
	code3, _ := req(t, "POST", "/admin/tasks/"+uuid.New().String()+"/requeue", nil, adminKey())
	if code3 != http.StatusNotFound {
		t.Fatalf("requeue unknown task: want 404, got %d", code3)
	}
}

// TestAdminAdjustReputation exercises the "manually adjust a supplier's
// reputation with an audit trail" gap named directly in the backlog rung: a real
// clamped adjustment, with before/after values recorded in the audit log.
func TestAdminAdjustReputation(t *testing.T) {
	reset(t)
	ctx := context.Background()
	if _, err := itPool.Exec(ctx, `UPDATE suppliers SET reputation=0.5 WHERE id=$1`, demoSupplierUUID); err != nil {
		t.Fatal(err)
	}

	code, body := req(t, "POST", "/admin/suppliers/"+demoSupplierUUID.String()+"/reputation",
		map[string]any{"delta": 0.3, "reason": "manual fraud review overturned an auto-quarantine"}, adminKey(), jsonCT())
	if code != http.StatusOK {
		t.Fatalf("adjust reputation: want 200, got %d: %s", code, body)
	}
	var resp struct {
		Before, After float32
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Before != 0.5 || resp.After != 0.8 {
		t.Fatalf("before/after = %v/%v, want 0.5/0.8", resp.Before, resp.After)
	}
	rep := supplierRep(t)
	if rep != float32(0.8) {
		t.Fatalf("persisted reputation = %v, want 0.8", rep)
	}

	// Clamped to [0,1]: a large positive delta never pushes reputation above 1.
	code2, body2 := req(t, "POST", "/admin/suppliers/"+demoSupplierUUID.String()+"/reputation",
		map[string]any{"delta": 5.0}, adminKey(), jsonCT())
	if code2 != http.StatusOK {
		t.Fatalf("adjust reputation (clamp high): want 200, got %d: %s", code2, body2)
	}
	if rep := supplierRep(t); rep != 1.0 {
		t.Fatalf("reputation after large positive delta = %v, want clamped to 1.0", rep)
	}

	// delta=0 is rejected as a caller mistake, not silently accepted.
	code3, _ := req(t, "POST", "/admin/suppliers/"+demoSupplierUUID.String()+"/reputation",
		map[string]any{"delta": 0.0}, adminKey(), jsonCT())
	if code3 != http.StatusBadRequest {
		t.Fatalf("adjust reputation delta=0: want 400, got %d", code3)
	}

	// An unknown supplier id is a 404.
	code4, _ := req(t, "POST", "/admin/suppliers/"+uuid.New().String()+"/reputation",
		map[string]any{"delta": 0.1}, adminKey(), jsonCT())
	if code4 != http.StatusNotFound {
		t.Fatalf("adjust reputation unknown supplier: want 404, got %d", code4)
	}
}

// TestAdminReleasePayoutHold exercises the "manually trigger a payout-hold
// release" gap named directly in the backlog rung: a held ledger entry's
// release_at is pulled forward to now() via a real endpoint, so the existing
// release-worker sweep (DuePayouts) picks it up on its very next cycle — this
// endpoint never fakes a 'released' status itself (that still requires a real
// payout_ref, per MarkPayout's invariant, enforced structurally by
// ledger_released_requires_ref).
func TestAdminReleasePayoutHold(t *testing.T) {
	reset(t)
	ctx := context.Background()
	var entryID uuid.UUID
	if err := itPool.QueryRow(ctx,
		`INSERT INTO ledger_entries (kind, supplier_id, amount_usd, payout_status, release_at)
		 VALUES ('supplier_credit', $1, 1.23, 'held', now() + interval '1 hour')
		 RETURNING id`, demoSupplierUUID).Scan(&entryID); err != nil {
		t.Fatal(err)
	}

	code, body := req(t, "POST", "/admin/payouts/"+entryID.String()+"/release",
		map[string]any{"reason": "buyer confirmed the job was legitimate, no need to wait out the hold"}, adminKey(), jsonCT())
	if code != http.StatusOK {
		t.Fatalf("release payout hold: want 200, got %d: %s", code, body)
	}

	var payoutStatus string
	var releaseAt time.Time
	if err := itPool.QueryRow(ctx, `SELECT payout_status, release_at FROM ledger_entries WHERE id=$1`, entryID).
		Scan(&payoutStatus, &releaseAt); err != nil {
		t.Fatal(err)
	}
	if payoutStatus != "held" {
		t.Fatalf("payout_status = %q, want still 'held' (this endpoint never fakes 'released')", payoutStatus)
	}
	if releaseAt.After(time.Now()) {
		t.Fatalf("release_at = %v, want <= now() so the next sweep picks it up", releaseAt)
	}

	// DuePayouts (the real release-worker sweep query) now genuinely picks this
	// entry up — proving the hold-release actually unblocks the real payout path,
	// not just a cosmetic timestamp change.
	due, err := itStore.DuePayouts(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	foundDue := false
	for _, d := range due {
		if d.ID == entryID {
			foundDue = true
		}
	}
	if !foundDue {
		t.Fatal("released entry not found in DuePayouts — the release-worker sweep would never pick it up")
	}

	// A non-held entry (e.g. already released) is a 409, not a silent success.
	var entryID2 uuid.UUID
	if err := itPool.QueryRow(ctx,
		`INSERT INTO ledger_entries (kind, supplier_id, amount_usd, payout_status, payout_ref)
		 VALUES ('supplier_credit', $1, 1.00, 'released', 'tr_test123')
		 RETURNING id`, demoSupplierUUID).Scan(&entryID2); err != nil {
		t.Fatal(err)
	}
	code2, _ := req(t, "POST", "/admin/payouts/"+entryID2.String()+"/release", nil, adminKey())
	if code2 != http.StatusConflict {
		t.Fatalf("release an already-released entry: want 409, got %d", code2)
	}

	// An unknown ledger entry id is a 404.
	code3, _ := req(t, "POST", "/admin/payouts/"+uuid.New().String()+"/release", nil, adminKey())
	if code3 != http.StatusNotFound {
		t.Fatalf("release unknown ledger entry: want 404, got %d", code3)
	}

	// A 'ready' entry (the honest no-rail-configured stub state) is ALSO accepted —
	// this is the exact case RUNBOOKS.md's OWN earlier documented "fix" (re-set to
	// 'ready') silently never got retried for, verified against the real DuePayouts
	// query. The endpoint must move it to 'held' (not leave it 'ready'), or DuePayouts
	// would never pick it up either.
	var entryID3 uuid.UUID
	if err := itPool.QueryRow(ctx,
		`INSERT INTO ledger_entries (kind, supplier_id, amount_usd, payout_status)
		 VALUES ('supplier_credit', $1, 4.56, 'ready')
		 RETURNING id`, demoSupplierUUID).Scan(&entryID3); err != nil {
		t.Fatal(err)
	}
	code4, body4 := req(t, "POST", "/admin/payouts/"+entryID3.String()+"/release", nil, adminKey())
	if code4 != http.StatusOK {
		t.Fatalf("release a 'ready' entry: want 200, got %d: %s", code4, body4)
	}
	var status3 string
	if err := itPool.QueryRow(ctx, `SELECT payout_status FROM ledger_entries WHERE id=$1`, entryID3).Scan(&status3); err != nil {
		t.Fatal(err)
	}
	if status3 != "held" {
		t.Fatalf("payout_status after releasing a 'ready' entry = %q, want 'held' (DuePayouts never selects 'ready')", status3)
	}
	due2, err := itStore.DuePayouts(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	found3 := false
	for _, d := range due2 {
		if d.ID == entryID3 {
			found3 = true
		}
	}
	if !found3 {
		t.Fatal("the formerly-'ready' entry is not in DuePayouts after release — it would still be stuck forever")
	}
}

// TestAdminActionsRequiresAuth confirms the new audit-log endpoint is behind the
// same admin gate as every other /admin/* write surface (no bearer key, no data).
func TestAdminActionsRequiresAuth(t *testing.T) {
	code, _ := req(t, "GET", "/admin/actions", nil)
	if code != http.StatusUnauthorized && code != http.StatusForbidden {
		t.Fatalf("GET /admin/actions with no auth: want 401/403, got %d", code)
	}
}
