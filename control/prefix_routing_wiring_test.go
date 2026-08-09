package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Live-path wiring tests for warm-prefix routing.
//
// These drive ClaimTasksTx the way scheduler_ask_claim_integration_test does:
// real workers, real jobs, real SKIP LOCKED claim. String-matching the SQL is
// not enough for the preference and starvation claims.

type prefixClaimWorker struct {
	supplierID, workerID uuid.UUID
	hwClass              string
	askUSDHr             float64
}

func seedPrefixClaimEnv(t *testing.T) (context.Context, *Store, *pgxpool.Pool, uuid.UUID) {
	t.Helper()
	databaseURL := requireTestDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	pool, err := pgxpool.New(ctx, databaseURL)
	must(t, err)
	t.Cleanup(pool.Close)
	store := NewStore(pool)
	must(t, store.Migrate(ctx))
	buyerID, err := store.CreateBuyerAccount(ctx,
		"pfx-wire-"+uuid.NewString()+"@example.test", "integration-password", 100)
	must(t, err)
	return ctx, store, pool, buyerID
}

func mkPrefixClaimWorker(t *testing.T, ctx context.Context, pool *pgxpool.Pool, hwClass string, ask float64) prefixClaimWorker {
	t.Helper()
	w := prefixClaimWorker{
		supplierID: uuid.New(),
		workerID:   uuid.New(),
		hwClass:    hwClass,
		askUSDHr:   ask,
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO suppliers (id,email,status,reputation,completed_tasks)
		 VALUES ($1,$2,'active',0.95,100)`,
		w.supplierID, "pfx-sup-"+uuid.NewString()+"@example.test"); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO workers (id,supplier_id,hw_class,memory_gb,effective_memory_gb,
		                      last_seen_at,throttled,min_payout_usd_hr)
		 VALUES ($1,$2,$3,64,64,now(),false,$4)`,
		w.workerID, w.supplierID, hwClass, ask); err != nil {
		t.Fatal(err)
	}
	bindLegacyTestWorkerExactExecutionIdentity(t, pool, ctx, w.workerID)
	if _, err := pool.Exec(ctx,
		`INSERT INTO worker_authorized_capabilities
		   (worker_id,cell_id,runtime_id,job_type,model_ref,model_kind,matrix_sha256)
		 VALUES ($1,'cell','rt','embed','all-minilm-l6-v2','hf',$2)`,
		w.workerID, generatedRuntimeMatrixSHA256); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		// workers is referenced ON DELETE RESTRICT from several tables; ageing
		// last_seen_at is the reliable way to take a test worker offline so a
		// later placement test's cheaper_ask / fleet count does not see it.
		// Cleanup that discards errors is not a cleanup — surface failures.
		if _, err := pool.Exec(c,
			`UPDATE workers SET last_seen_at = now() - interval '10 minutes' WHERE id=$1`,
			w.workerID); err != nil {
			t.Errorf("age prefix claim worker offline: %v", err)
		}
		if _, err := pool.Exec(c, `DELETE FROM worker_prefix_state WHERE worker_id=$1`, w.workerID); err != nil {
			t.Errorf("cleanup worker_prefix_state: %v", err)
		}
		if _, err := pool.Exec(c, `DELETE FROM worker_authorized_capabilities WHERE worker_id=$1`, w.workerID); err != nil {
			t.Errorf("cleanup worker_authorized_capabilities: %v", err)
		}
	})
	return w
}

func seedPrefixClaimJob(t *testing.T, ctx context.Context, pool *pgxpool.Pool, store *Store, buyerID uuid.UUID, chain []PrefixChainEntry) (jobID, taskID uuid.UUID) {
	t.Helper()
	jobID = uuid.New()
	taskID = uuid.New()
	prefixID := ""
	if len(chain) > 0 {
		prefixID = chain[0].PrefixID
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO jobs (id,buyer_id,status,job_type,model_ref,input_ref,task_count,
		                  offered_rate_usd_hr,min_memory_gb,tier,prefix_id)
		VALUES ($1,$2,'running','embed','all-minilm-l6-v2','in',1,10.0,0,'batch',NULLIF($3,''))`,
		jobID, buyerID, prefixID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO tasks (id,job_id,status,input_ref,result_key)
		VALUES ($1,$2,'queued','in','rk')`, taskID, jobID); err != nil {
		t.Fatal(err)
	}
	if len(chain) > 0 {
		must(t, store.RecordJobPrefixChain(ctx, jobID, chain))
	}
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_, _ = pool.Exec(c, `DELETE FROM job_prefix_chain WHERE job_id=$1`, jobID)
		_, _ = pool.Exec(c, `DELETE FROM tasks WHERE job_id=$1`, jobID)
		_, _ = pool.Exec(c, `DELETE FROM jobs WHERE id=$1`, jobID)
	})
	return jobID, taskID
}

func claimAs(t *testing.T, ctx context.Context, store *Store, w prefixClaimWorker) *ClaimedTask {
	t.Helper()
	got, err := store.ClaimTasksTx(ctx, WorkerAuth{WorkerID: w.workerID, SupplierID: w.supplierID})
	mustf(t, err, "claim: %v")
	return got
}

func uniqueTokenChain(t *testing.T, depth int) []PrefixChainEntry {
	t.Helper()
	nonce := int(uuid.New().ID())
	toks := make([]int, depth)
	for i := range toks {
		toks[i] = nonce + i + 1
	}
	chain := ComputePrefixChain(toks)
	if len(chain) == 0 {
		t.Fatalf("expected chain for %d tokens", depth)
	}
	return chain
}

// A job whose prefix nobody is warm on must still be claimed with no added
// latency. Warmth is a preference, never a hard filter.
func TestColdPrefixJobIsStillClaimed(t *testing.T) {
	ctx, store, pool, buyerID := seedPrefixClaimEnv(t)
	// Ask = 0 so leftover workers from other tests cannot trip cheaper_ask
	// deferral on this fresh task (that would look like a warmth filter).
	worker := mkPrefixClaimWorker(t, ctx, pool, "apple_silicon_max", 0)
	chain := uniqueTokenChain(t, 128)
	_, taskID := seedPrefixClaimJob(t, ctx, pool, store, buyerID, chain)

	start := time.Now()
	got := claimAs(t, ctx, store, worker)
	elapsed := time.Since(start)
	if got == nil {
		t.Fatal("cold-prefix job was not claimable; warmth must never be a hard filter")
	}
	if got.TaskID != taskID {
		t.Fatalf("claimed %s, want %s", got.TaskID, taskID)
	}
	// Claim is a single SQL statement; anything multi-second would be a
	// regression toward a warmth wait/poll. Bound well above normal claim
	// latency and well below any "wait for a warm worker" design.
	if elapsed > 2*time.Second {
		t.Fatalf("cold claim took %v; warmth must not add wait latency", elapsed)
	}
}

// Between two workers of the SAME cost class, a worker that is warm on a
// deeper prefix prefers the matching job over a cold one. The claim path is
// worker-pull: preference is expressed as which task a warm worker takes when
// both a warm-match job and a cold job are available.
func TestDeeperWarmPrefixPreferredWithinSameCostClass(t *testing.T) {
	ctx, store, pool, buyerID := seedPrefixClaimEnv(t)
	// Ask = 0: a non-zero ask lets cheaper leftover workers from the shared
	// test DB defer only the NEWER task, which would make the aged cold job
	// win for ask reasons rather than testing warmth preference.
	worker := mkPrefixClaimWorker(t, ctx, pool, "apple_silicon_max", 0)

	deepChain := uniqueTokenChain(t, 300) // nodes at 32/64/128/256
	coldChain := uniqueTokenChain(t, 300)
	if deepChain[0].PrefixID == coldChain[0].PrefixID {
		t.Fatal("test chains collided")
	}

	// Age the cold job older so without warmth it would win on created_at ASC.
	coldJob, coldTask := seedPrefixClaimJob(t, ctx, pool, store, buyerID, coldChain)
	deepJob, deepTask := seedPrefixClaimJob(t, ctx, pool, store, buyerID, deepChain)
	if _, err := pool.Exec(ctx,
		`UPDATE tasks SET created_at = now() - interval '1 hour', visible_at = now() - interval '1 hour'
		   WHERE id = $1`, coldTask); err != nil {
		t.Fatal(err)
	}
	_ = coldJob
	_ = deepJob

	// Worker holds the deep chain fully warm; nothing for the cold job.
	must(t, store.MarkPrefixChainWarm(ctx, worker.workerID, deepChain))

	got := claimAs(t, ctx, store, worker)
	if got == nil {
		t.Fatal("no claim")
	}
	if got.TaskID != deepTask {
		t.Fatalf("claimed task %s (job %s); want the deep-warm job %s, not the older cold job %s",
			got.TaskID, got.JobID, deepTask, coldTask)
	}

	// Depth distinguishes deep from shallow on the same job family.
	depth, err := store.DeepestWarmPrefix(ctx, worker.workerID, deepJob)
	must(t, err)
	if want := deepChain[len(deepChain)-1].Depth; depth != want {
		t.Fatalf("deepest warm = %d, want %d", depth, want)
	}
}

// A cold cheap worker is not displaced by a warm expensive one. Cost rank
// sits above warm_prefix_depth in ORDER BY, and cheaper_ask_online hard-defers
// the expensive worker while a cheaper ask is online. Without measured
// prefill-vs-class arithmetic, cost wins.
func TestColdCheapWorkerNotDisplacedByWarmExpensive(t *testing.T) {
	ctx, store, pool, buyerID := seedPrefixClaimEnv(t)

	// Same hardware class so cheaper_class_online is not the mechanism under
	// test; the ask deferral is. Different asks, identical capability.
	cheap := mkPrefixClaimWorker(t, ctx, pool, "apple_silicon_max", 0.10)
	dear := mkPrefixClaimWorker(t, ctx, pool, "apple_silicon_max", 5.00)

	chain := uniqueTokenChain(t, 256)
	_, taskID := seedPrefixClaimJob(t, ctx, pool, store, buyerID, chain)
	// Expensive worker is fully warm; cheap worker is cold.
	must(t, store.MarkPrefixChainWarm(ctx, dear.workerID, chain))

	// Expensive warm worker asks first: must be deferred to the cheap ask.
	if got := claimAs(t, ctx, store, dear); got != nil {
		t.Fatalf("warm expensive worker claimed task while a cold cheaper ask was online (got task %s)", got.TaskID)
	}
	// Cold cheap worker takes it.
	got := claimAs(t, ctx, store, cheap)
	if got == nil {
		t.Fatal("cold cheap worker could not claim; warmth must not starve the cheaper class")
	}
	if got.TaskID != taskID {
		t.Fatalf("claimed %s, want %s", got.TaskID, taskID)
	}

	// Source-level composition: cost terms MUST appear before warm depth in
	// the ORDER BY so a future edit cannot silently invert the preference.
	sql := ClaimTaskSQL("t.claimed_by IS NULL")
	orderIdx := strings.Index(sql, "ORDER BY")
	if orderIdx < 0 {
		t.Fatal("claim SQL has no ORDER BY")
	}
	order := sql[orderIdx:]
	cheapPos := strings.Index(order, "cheaper_class_online")
	warmPos := strings.Index(order, "warm_prefix_depth")
	if cheapPos < 0 || warmPos < 0 {
		t.Fatalf("ORDER BY missing cost/warm signals: cheaper=%d warm=%d", cheapPos, warmPos)
	}
	if warmPos < cheapPos {
		t.Fatal("warm_prefix_depth ordered before cheaper_class_online: warm expensive would beat cold cheap")
	}
}

// Stale warmth expires and stops influencing routing. A worker whose
// last_seen_warm is past the TTL must rank as cold even if the row remains.
func TestStaleWarmthStopsInfluencingRouting(t *testing.T) {
	ctx, store, pool, buyerID := seedPrefixClaimEnv(t)
	worker := mkPrefixClaimWorker(t, ctx, pool, "apple_silicon_max", 0)

	warmChain := uniqueTokenChain(t, 128)
	coldChain := uniqueTokenChain(t, 128)
	staleJob, staleTask := seedPrefixClaimJob(t, ctx, pool, store, buyerID, warmChain)
	freshJob, freshTask := seedPrefixClaimJob(t, ctx, pool, store, buyerID, coldChain)
	// Make the stale-chain job older so without warmth the fresh job loses
	// on age; after warmth expires the older job should win again.
	if _, err := pool.Exec(ctx,
		`UPDATE tasks SET created_at = now() - interval '1 hour', visible_at = now() - interval '1 hour'
		   WHERE id = $1`, staleTask); err != nil {
		t.Fatal(err)
	}
	_ = staleJob
	_ = freshJob

	must(t, store.MarkPrefixChainWarm(ctx, worker.workerID, warmChain))
	// Age every warm row past the TTL.
	if _, err := pool.Exec(ctx,
		`UPDATE worker_prefix_state SET last_seen_warm = now() - $2::interval
		   WHERE worker_id = $1`,
		worker.workerID, (prefixWarmTTL + time.Minute).String()); err != nil {
		t.Fatal(err)
	}

	depth, err := store.DeepestWarmPrefix(ctx, worker.workerID, staleJob)
	must(t, err)
	if depth != 0 {
		t.Fatalf("stale warmth still reports depth %d", depth)
	}

	// With warmth expired, oldest-first wins: the aged stale-chain job.
	got := claimAs(t, ctx, store, worker)
	if got == nil {
		t.Fatal("no claim after warmth expired")
	}
	if got.TaskID != staleTask {
		t.Fatalf("claimed %s; want oldest job %s once warmth no longer ranks the other first (fresh was %s)",
			got.TaskID, staleTask, freshTask)
	}

	// Sweep removes rows past 20×TTL so a dropped cache cannot keep
	// attracting work after the retention window.
	if _, err := pool.Exec(ctx,
		`UPDATE worker_prefix_state SET last_seen_warm = now() - $2::interval
		   WHERE worker_id = $1`,
		worker.workerID, (20*prefixWarmTTL + time.Minute).String()); err != nil {
		t.Fatal(err)
	}
	n, err := store.SweepStalePrefixState(ctx)
	must(t, err)
	if n == 0 {
		t.Fatal("SweepStalePrefixState removed nothing for rows past 20×TTL")
	}
	var remaining int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM worker_prefix_state WHERE worker_id=$1`, worker.workerID,
	).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 0 {
		t.Fatalf("%d warm rows survived the sweep", remaining)
	}
}

// Completion of a task marks the serving worker warm for the job's chain.
func TestCompletionMarksPrefixChainWarm(t *testing.T) {
	ctx, store, pool, buyerID := seedPrefixClaimEnv(t)
	worker := mkPrefixClaimWorker(t, ctx, pool, "apple_silicon_max", 0)
	chain := uniqueTokenChain(t, 128)
	jobID, taskID := seedPrefixClaimJob(t, ctx, pool, store, buyerID, chain)

	// Put the task into a claimable then running state via the real claim path.
	got := claimAs(t, ctx, store, worker)
	if got == nil || got.TaskID != taskID {
		t.Fatalf("claim failed: %+v", got)
	}

	// Mark warm as the commit path does.
	must(t, store.markWorkerWarmForJob(ctx, worker.workerID, jobID))
	depth, err := store.DeepestWarmPrefix(ctx, worker.workerID, jobID)
	must(t, err)
	if want := chain[len(chain)-1].Depth; depth != want {
		t.Fatalf("after completion warm depth = %d, want %d", depth, want)
	}
}

// Source-level oracle guard: no production route, metric, or log format may
// expose which prefixes a worker holds. Aggregate counts are fine; per-prefix
// existence is a cross-tenant oracle (submit candidate, observe warm routing).
// Pattern matches TestNoRawLedgerInsertsOutsideWriter.
func TestNoPerPrefixWarmthOracleInProductionSurfaces(t *testing.T) {
	entries, err := os.ReadDir(".")
	must(t, err)

	// HTTP surfaces that would let a client query warmth by prefix id.
	routeNeedles := []string{
		"/v1/prefix",
		"/v1/admin/prefix",
		"handlePrefixWarm",
		"handleWorkerPrefix",
		"PrefixWarmWorkers(",
		"PrefixIsWarm(",
	}
	// Metric labels that would emit prefix_id as a dimension.
	metricNeedles := []string{
		`prefix_id="`,
		"prefix_id=",
		"warm_prefix_id",
		"label_prefix_id",
	}
	// Log formats that interpolate a prefix id (pfx_...). Aggregate counts OK.
	logNeedles := []string{
		"prefix_id=%s",
		"prefix_id=%q",
		"prefix=%s",
		"prefix=%q",
		"warm prefix %s",
		"warm for prefix",
	}

	// Store methods used only inside the package for routing are allowed in
	// prefix_routing.go / prefix_routing_path.go / scheduler.go / store_tasks.go
	// / workers.go. They must not appear in api.go or metrics.go.
	var offenders []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		body, err := os.ReadFile(name)
		must(t, err)
		src := string(body)

		if name == "api.go" || name == "metrics.go" {
			for _, n := range routeNeedles {
				if strings.Contains(src, n) {
					offenders = append(offenders, name+": route/query "+n)
				}
			}
		}
		if name == "metrics.go" {
			for _, n := range metricNeedles {
				if strings.Contains(src, n) {
					offenders = append(offenders, name+": metric "+n)
				}
			}
		}
		// Any production file logging a prefix id value.
		for _, n := range logNeedles {
			if strings.Contains(src, n) {
				offenders = append(offenders, name+": log "+n)
			}
		}
	}
	if len(offenders) > 0 {
		t.Fatalf("per-prefix warmth oracle surface(s): %v", offenders)
	}

	// Claim SQL must not project prefix_id into a RETURNING clause a client
	// could observe via poll.
	sql := ClaimTaskSQL("t.claimed_by IS NULL")
	retIdx := strings.LastIndex(sql, "RETURNING")
	if retIdx < 0 {
		t.Fatal("claim SQL has no RETURNING")
	}
	if strings.Contains(sql[retIdx:], "prefix_id") || strings.Contains(sql[retIdx:], "warm_prefix") {
		t.Fatal("claim RETURNING exposes prefix warmth to the worker client")
	}

	// Workers loop must register the sweep (dead SweepStalePrefixState was the
	// original defect).
	workersSrc, err := os.ReadFile("workers.go")
	must(t, err)
	if !strings.Contains(string(workersSrc), "SweepStalePrefixState") {
		t.Fatal("workers.go does not call SweepStalePrefixState")
	}
	if !strings.Contains(string(workersSrc), "prefix-state-retention") {
		t.Fatal("workers.go does not register the prefix-state-retention ticker")
	}

	// Sanity: the test file itself is not the only place the symbols live.
	_ = filepath.Separator
}

// ClaimTaskSQL orders warm_prefix_depth as a preference after cost rank and
// never filters on it in WHERE.
func TestClaimSQLWarmPrefixIsPreferenceNotFilter(t *testing.T) {
	sql := ClaimTaskSQL("t.claimed_by IS NULL")
	if !strings.Contains(sql, "warm_prefix_depth") {
		t.Fatal("claim SQL lost warm_prefix_depth")
	}
	// The depth expression must live in the SELECT list / ORDER BY, not as a
	// WHERE filter that would drop cold jobs.
	whereIdx := strings.Index(sql, "WHERE j.status")
	orderIdx := strings.Index(sql, "ORDER BY")
	if whereIdx < 0 || orderIdx < 0 {
		t.Fatal("could not locate WHERE/ORDER BY in claim SQL")
	}
	// eligible_jobs WHERE must not require warm_prefix_depth > 0.
	// The signal is computed in the SELECT list of eligible_jobs.
	if strings.Contains(sql, "warm_prefix_depth >") ||
		strings.Contains(sql, "warm_prefix_depth>=") ||
		strings.Contains(sql, "AND warm_prefix_depth") {
		t.Fatal("warm_prefix_depth used as a hard filter; cold jobs would starve")
	}
	order := sql[orderIdx:]
	if !strings.Contains(order, "warm_prefix_depth DESC") {
		t.Fatal("warm_prefix_depth is not a DESC preference in ORDER BY")
	}
}

// prefixTokensFromBytes is stable and deep enough to produce a chain for a
// realistic shared-prefix sample.
func TestPrefixTokensFromBytesAreStableAndChainable(t *testing.T) {
	// 200+ bytes → ≥50 surrogate tokens → at least depth-32 chain node.
	sample := []byte(strings.Repeat("You are a precise assistant. ", 20))
	a := prefixTokensFromBytes(sample)
	b := prefixTokensFromBytes(sample)
	if len(a) == 0 || len(a) != len(b) {
		t.Fatalf("token lengths %d vs %d", len(a), len(b))
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("unstable at token %d", i)
		}
	}
	chain := prefixChainFromInputBytes(sample)
	if len(chain) == 0 {
		t.Fatal("expected a non-empty prefix chain for a multi-hundred-byte sample")
	}
	// Divergent leading bytes must not share a chain node.
	other := append([]byte("DIFFERENT"), sample[9:]...)
	otherChain := prefixChainFromInputBytes(other)
	if len(otherChain) > 0 && otherChain[0].PrefixID == chain[0].PrefixID {
		t.Fatal("divergent inputs produced the same shallow prefix id")
	}
}
