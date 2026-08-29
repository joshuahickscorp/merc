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

// ── Test 1: seatbelt profile is deny-default without wildcard egress ─────────

func TestSeatbeltProfileIsDenyDefaultWithoutWildcardEgress(t *testing.T) {
	// Parse the profile file so a future edit that re-opens allow-default or
	// *:443 fails the suite.
	candidates := []string{
		filepath.Join("..", "..", "clients", "macapp", "ComputeExchangeAgent", "merc-agent.sb"),
	}
	var body []byte
	var err error
	for _, p := range candidates {
		body, err = os.ReadFile(p)
		if err == nil {
			break
		}
	}
	mustf(t, err, "merc-agent.sb not found: %v")
	// Strip line comments so explanatory prose cannot trip the string gates.
	var live strings.Builder
	for _, line := range strings.Split(string(body), "\n") {
		if i := strings.Index(line, ";;"); i >= 0 {
			line = line[:i]
		}
		live.WriteString(line)
		live.WriteByte('\n')
	}
	text := live.String()
	if !strings.Contains(text, "(deny default)") {
		t.Fatal("seatbelt profile is missing (deny default) — containment requires deny-default")
	}
	if strings.Contains(text, "(allow default)") {
		t.Fatal("seatbelt profile still contains (allow default) — re-opens every unlisted class")
	}
	// Wildcard host egress on 80/443 was the exfil path.
	for _, bad := range []string{
		`(remote ip "*:443")`,
		`(remote ip "*:80")`,
		`(remote tcp "*:443")`,
		`(remote tcp "*:80")`,
	} {
		if strings.Contains(text, bad) {
			t.Fatalf("seatbelt profile contains wildcard egress %s — buyer payload could exfiltrate to any host", bad)
		}
	}
	// Public sandbox-exec cannot host-filter remote TCP; remote peers are
	// reached via a loopback CONNECT proxy. The profile must document that and
	// still carry BINDIR for the installed agent binary.
	if !strings.Contains(text, "BINDIR") {
		t.Fatal("seatbelt profile missing BINDIR param for the agent install prefix")
	}
	if !strings.Contains(text, "localhost") {
		t.Fatal("seatbelt profile must allow localhost (egress proxy / local control plane)")
	}
}

// ── Test 2 helpers live in the agent package; control plane checks capability record ──

func TestUnsandboxedCapabilityIsRecordedOnRegister(t *testing.T) {
	// Enrolment projects against directed cells. Production evidence is correctly
	// unbindable for ordinary routing, but document lifecycle still leaves embed
	// directed-reachable. Install TEST_ONLY publication before Migrate, then
	// re-clear the activation overlay after the shared-DB load so suite order
	// cannot leave the cell quarantined.
	installBoundCataloguePublicationAuthorityForTest(t)
	ctx, store, pool := openPayoutTestStore(t)
	installed := currentActivation()
	activeRuntimeActivation.Store(newRuntimeActivation(
		installed.PolicyRevision, map[string]string{}, nil))
	supplierID, workerID := uuid.New(), uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO suppliers (id,email,status) VALUES ($1,$2,'active')`,
		supplierID, "unsandbox-"+uuid.NewString()+"@corp.example"); err != nil {
		t.Fatal(err)
	}
	registerContainmentCleanup(t, pool, []uuid.UUID{workerID}, nil)
	if _, err := store.CreateWorkerToken(ctx, workerID, supplierID); err != nil {
		t.Fatal(err)
	}
	cap := testWorkerCapability(workerID, supplierID)
	cap.Sandboxed = false
	cap.UnsandboxedOptIn = true
	mustf(t, store.UpsertWorker(ctx, cap), "register unsandboxed capability: %v")
	var sandboxed, optIn bool
	if err := pool.QueryRow(ctx,
		`SELECT sandboxed, unsandboxed_opt_in FROM workers WHERE id=$1`, workerID,
	).Scan(&sandboxed, &optIn); err != nil {
		t.Fatal(err)
	}
	if sandboxed {
		t.Fatal("capability record says sandboxed=true for an unsandboxed opt-in worker")
	}
	if !optIn {
		t.Fatal("capability record missing unsandboxed_opt_in=true — control plane cannot refuse private work")
	}
}

// ── Test 3: expired worker token rejected; renewed accepted ──────────────────

func TestExpiredWorkerTokenIsRejectedAndRenewedIsAccepted(t *testing.T) {
	ctx, store, pool := openPayoutTestStore(t)
	supplierID, workerID := uuid.New(), uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO suppliers (id,email,status) VALUES ($1,$2,'active')`,
		supplierID, "token-"+uuid.NewString()+"@corp.example"); err != nil {
		t.Fatal(err)
	}
	registerContainmentCleanup(t, pool, []uuid.UUID{workerID}, nil)
	raw, expires, err := store.CreateWorkerTokenWithExpiry(ctx, workerID, supplierID, 2*time.Hour)
	must(t, err)
	if expires.Before(time.Now().Add(time.Hour)) {
		t.Fatalf("new token expiry %s is shorter than expected TTL", expires)
	}
	auth, err := store.LookupWorkerToken(ctx, raw)
	mustf(t, err, "fresh token rejected: %v")
	if auth.WorkerID != workerID {
		t.Fatalf("worker_id=%s want %s", auth.WorkerID, workerID)
	}

	// Expire the token.
	if _, err := pool.Exec(ctx,
		`UPDATE worker_tokens SET expires_at = now() - interval '1 second' WHERE credential_id=$1`,
		auth.CredentialID); err != nil {
		t.Fatal(err)
	}
	_, err = store.LookupWorkerToken(ctx, raw)
	if err == nil {
		t.Fatal("expired worker token was accepted")
	}
	if !strings.Contains(err.Error(), "WORKER_TOKEN_EXPIRED") {
		t.Fatalf("expired token error %q missing greppable WORKER_TOKEN_EXPIRED", err)
	}

	// Renewal requires a non-expired credential; re-issue and renew.
	raw2, _, err := store.CreateWorkerTokenWithExpiry(ctx, workerID, supplierID, time.Minute)
	must(t, err)
	auth2, err := store.LookupWorkerToken(ctx, raw2)
	must(t, err)
	newExp, err := store.RenewWorkerToken(ctx, auth2.CredentialID)
	mustf(t, err, "renew: %v")
	if !newExp.After(time.Now().Add(time.Hour)) {
		t.Fatalf("renewed expiry %s did not extend by roughly workerTokenTTL", newExp)
	}
	if _, err := store.LookupWorkerToken(ctx, raw2); err != nil {
		t.Fatalf("renewed token rejected: %v", err)
	}
}

// ── Test 4: linked supplier cannot claim buyer's task; exclusion recorded ────

func TestLinkedSupplierCannotClaimBuyerTaskAndExclusionIsRecorded(t *testing.T) {
	ctx, store, pool := openContainmentTestStore(t)
	suffix := uuid.NewString()
	buyerID, err := store.CreateBuyerAccount(ctx, "buyer-"+suffix+"@corp.example", "pw", 100)
	must(t, err)
	// Supplier owned by the buyer — primary link signal.
	supplierID, workerID := uuid.New(), uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO suppliers (id,email,owner_buyer_id,status,reputation,completed_tasks)
		 VALUES ($1,$2,$3,'active',0.95,100)`,
		supplierID, "sup-"+suffix+"@corp.example", buyerID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO workers (id,supplier_id,hw_class,memory_gb,effective_memory_gb,
		                      last_seen_at,throttled,min_payout_usd_hr)
		 VALUES ($1,$2,'apple_silicon_max',64,64,now(),false,0)`,
		workerID, supplierID); err != nil {
		t.Fatal(err)
	}
	bindWorkerToGovernedProfile(t, pool, ctx, workerID)
	if _, err := pool.Exec(ctx,
		`INSERT INTO worker_authorized_capabilities
		   (worker_id,cell_id,runtime_id,job_type,model_ref,model_kind,matrix_sha256,routable)
		 VALUES ($1,'cell','rt','embed','all-minilm-l6-v2','hf',$2,true)`,
		workerID, generatedRuntimeMatrixSHA256); err != nil {
		t.Fatal(err)
	}

	jobID, taskID := uuid.New(), uuid.New()
	registerContainmentCleanup(t, pool, []uuid.UUID{workerID}, []uuid.UUID{jobID})
	if _, err := pool.Exec(ctx, `
		INSERT INTO jobs (id,buyer_id,status,job_type,model_ref,input_ref,task_count,
		                  offered_rate_usd_hr,min_memory_gb,tier)
		VALUES ($1,$2,'running','embed','all-minilm-l6-v2','in',1,10.0,0,'batch')`,
		jobID, buyerID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO tasks (id,job_id,status,input_ref,result_key)
		VALUES ($1,$2,'queued','in','rk')`, taskID, jobID); err != nil {
		t.Fatal(err)
	}

	linked, signals, err := store.SupplierLinkedToBuyer(ctx, buyerID, supplierID)
	must(t, err)
	if !linked {
		t.Fatal("owner_buyer_id link was not detected")
	}
	if !containsStr(signals, "owner_buyer_id") {
		t.Fatalf("signals=%v missing owner_buyer_id", signals)
	}

	got, err := store.ClaimTasksTx(ctx, WorkerAuth{WorkerID: workerID, SupplierID: supplierID})
	mustf(t, err, "claim: %v")
	// Shared DB may have other buyers' work; only THIS buyer's task must be refused.
	if got != nil && got.TaskID == taskID {
		t.Fatalf("linked supplier claimed buyer task %s — self-dealing path is open", got.TaskID)
	}
	if got != nil && got.JobID == jobID {
		t.Fatalf("linked supplier claimed a task on the buyer's job %s", got.JobID)
	}
	var status string
	var claimedBy *uuid.UUID
	if err := pool.QueryRow(ctx,
		`SELECT status, claimed_by FROM tasks WHERE id=$1`, taskID,
	).Scan(&status, &claimedBy); err != nil {
		t.Fatal(err)
	}
	if status != "queued" || claimedBy != nil {
		t.Fatalf("buyer's task status=%s claimed_by=%v; linked supplier must not take it", status, claimedBy)
	}

	var n int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM claim_independence_exclusions
		 WHERE job_id=$1 AND supplier_id=$2 AND exclusion_kind='buyer_work'`,
		jobID, supplierID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		t.Fatal("placement decision did not record the independence exclusion")
	}
}

// ── Test 5: linked supplier cannot claim redundancy/honeypot even if ordinary work allowed ──
// (Here we use enrollment link which is the same hard exclusion for all work;
//  verification_kind is recorded when the only ready tasks are verification.)

func TestLinkedSupplierCannotClaimVerificationTasks(t *testing.T) {
	ctx, store, pool := openContainmentTestStore(t)
	suffix := uuid.NewString()
	buyerID, err := store.CreateBuyerAccount(ctx, "vbuyer-"+suffix+"@corp.example", "pw", 100)
	must(t, err)
	supplierID, workerID := uuid.New(), uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO suppliers (id,email,owner_buyer_id,status,reputation,completed_tasks)
		 VALUES ($1,$2,$3,'active',0.95,100)`,
		supplierID, "vsup-"+suffix+"@corp.example", buyerID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO workers (id,supplier_id,hw_class,memory_gb,effective_memory_gb,
		                      last_seen_at,throttled,min_payout_usd_hr)
		 VALUES ($1,$2,'apple_silicon_max',64,64,now(),false,0)`,
		workerID, supplierID); err != nil {
		t.Fatal(err)
	}
	bindWorkerToGovernedProfile(t, pool, ctx, workerID)
	if _, err := pool.Exec(ctx,
		`INSERT INTO worker_authorized_capabilities
		   (worker_id,cell_id,runtime_id,job_type,model_ref,model_kind,matrix_sha256,routable)
		 VALUES ($1,'cell','rt','embed','all-minilm-l6-v2','hf',$2,true)`,
		workerID, generatedRuntimeMatrixSHA256); err != nil {
		t.Fatal(err)
	}

	jobID, honeypotID, redunID := uuid.New(), uuid.New(), uuid.New()
	registerContainmentCleanup(t, pool, []uuid.UUID{workerID}, []uuid.UUID{jobID})
	if _, err := pool.Exec(ctx, `
		INSERT INTO jobs (id,buyer_id,status,job_type,model_ref,input_ref,task_count,
		                  offered_rate_usd_hr,min_memory_gb,tier)
		VALUES ($1,$2,'running','embed','all-minilm-l6-v2','in',2,10.0,0,'batch')`,
		jobID, buyerID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO tasks (id,job_id,status,input_ref,result_key,is_honeypot)
		VALUES ($1,$2,'queued','in','rk-h',true)`, honeypotID, jobID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO tasks (id,job_id,status,input_ref,result_key,is_redundancy)
		VALUES ($1,$2,'queued','in','rk-r',true)`, redunID, jobID); err != nil {
		t.Fatal(err)
	}

	got, err := store.ClaimTasksTx(ctx, WorkerAuth{WorkerID: workerID, SupplierID: supplierID})
	mustf(t, err, "claim: %v")
	if got != nil && (got.TaskID == honeypotID || got.TaskID == redunID || got.JobID == jobID) {
		t.Fatalf("linked supplier claimed verification task %s on job %s", got.TaskID, got.JobID)
	}
	for _, id := range []uuid.UUID{honeypotID, redunID} {
		var status string
		var claimedBy *uuid.UUID
		if err := pool.QueryRow(ctx,
			`SELECT status, claimed_by FROM tasks WHERE id=$1`, id,
		).Scan(&status, &claimedBy); err != nil {
			t.Fatal(err)
		}
		if status != "queued" || claimedBy != nil {
			t.Fatalf("verification task %s status=%s claimed_by=%v", id, status, claimedBy)
		}
	}
	var kind string
	if err := pool.QueryRow(ctx, `
		SELECT exclusion_kind FROM claim_independence_exclusions
		 WHERE job_id=$1 AND supplier_id=$2
		 ORDER BY created_at DESC LIMIT 1`,
		jobID, supplierID).Scan(&kind); err != nil {
		t.Fatalf("exclusion not recorded: %v", err)
	}
	if kind != "verification_work" {
		t.Fatalf("exclusion_kind=%q want verification_work", kind)
	}
}

// ── Test 6: job with no independent supplier is refused, not settled ─────────

func TestNoIndependentSupplierIsRefusedNotSettled(t *testing.T) {
	t.Parallel()
	// Platform-wide online-supplier count must not be polluted by sibling tests.
	ctx, store, pool := openIsolatedTestStore(t)
	suffix := uuid.NewString()
	buyerID, err := store.CreateBuyerAccount(ctx, "alone-"+suffix+"@corp.example", "pw", 100)
	must(t, err)
	// Only a linked supplier is online.
	supplierID, workerID := uuid.New(), uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO suppliers (id,email,owner_buyer_id,status,reputation,completed_tasks)
		 VALUES ($1,$2,$3,'active',0.95,100)`,
		supplierID, "alone-sup-"+suffix+"@corp.example", buyerID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO workers (id,supplier_id,hw_class,memory_gb,effective_memory_gb,
		                      last_seen_at,throttled,min_payout_usd_hr)
		 VALUES ($1,$2,'apple_silicon_max',64,64,now(),false,0)`,
		workerID, supplierID); err != nil {
		t.Fatal(err)
	}
	jobID := uuid.New()
	registerContainmentCleanup(t, pool, []uuid.UUID{workerID}, []uuid.UUID{jobID})
	if _, err := pool.Exec(ctx, `
		INSERT INTO jobs (id,buyer_id,status,job_type,model_ref,input_ref,task_count,tier)
		VALUES ($1,$2,'running','embed','all-minilm-l6-v2','in',1,'batch')`,
		jobID, buyerID); err != nil {
		t.Fatal(err)
	}
	err = store.RefuseIfNoIndependentSupplier(ctx, jobID, buyerID, true)
	if err == nil {
		t.Fatal("job requiring verification was accepted with only a linked supplier online")
	}
	if !strings.Contains(err.Error(), "NO_INDEPENDENT_SUPPLIER") {
		t.Fatalf("error %q missing greppable NO_INDEPENDENT_SUPPLIER", err)
	}
	var n int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM claim_independence_exclusions
		 WHERE job_id=$1 AND exclusion_kind='no_independent_supplier'`,
		jobID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		t.Fatal("no_independent_supplier exclusion was not recorded")
	}
}

// ── Test 7: disputed vs unpeered benchmark rates ─────────────────────────────
//
// The floor is for cells that *could* be corroborated and were not — peers
// exist in the class and none agree. A cell with no peer is unpeered, not
// uncorroborated; flooring it would make a single-supplier fleet unroutable
// and invert cost rank against every fixture that seeds one worker.

func TestUncorroboratedBenchmarkRatePolicy(t *testing.T) {
	if !ratesAgreeWithinTolerance(100, 110, benchmarkCorroborationTolerance) {
		t.Fatal("100 vs 110 should agree within 25% policy tolerance")
	}
	if ratesAgreeWithinTolerance(100, 200, benchmarkCorroborationTolerance) {
		t.Fatal("100 vs 200 must not corroborate under 25% tolerance")
	}
	// Disputed (peers present, not agreed): floor.
	if got := RoutableBenchmarkRate(500, false, true); got != uncorroboratedBenchmarkFloorTPS {
		t.Fatalf("disputed rate=%v want floor %v", got, uncorroboratedBenchmarkFloorTPS)
	}
	// Unpeered: provisional claimed rate, not the floor.
	if got := RoutableBenchmarkRate(500, false, false); got != 500 {
		t.Fatalf("unpeered rate=%v want claimed 500", got)
	}
	// Corroborated: claimed rate regardless of peerAvailable flag.
	if got := RoutableBenchmarkRate(500, true, true); got != 500 {
		t.Fatalf("corroborated rate=%v want 500", got)
	}
}

func TestUnpeeredBenchmarkKeepsClaimedRate(t *testing.T) {
	// Isolated DB: residual fleet benchmarks for all-minilm would look like peers
	// and turn this unpeered case into a dispute on a shared database.
	installBoundCataloguePublicationAuthorityForTest(t)
	ctx, store, pool := openIsolatedTestStore(t)
	supplierID, workerID := uuid.New(), uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO suppliers (id,email,status) VALUES ($1,$2,'active')`,
		supplierID, "bench-"+uuid.NewString()+"@corp.example"); err != nil {
		t.Fatal(err)
	}
	registerContainmentCleanup(t, pool, []uuid.UUID{workerID}, nil)
	cap := testWorkerCapability(workerID, supplierID)
	// Self-report with no peer in the cell class: unpeered, not disputed.
	// Rate must clear the TEST_ONLY publication floor while remaining far above
	// any honest peer the disputed case will introduce.
	cap.Benchmarks = []BenchResult{{
		ModelID: "all-minilm-l6-v2", JobType: "embed",
		EPS: 9999, TPS: 0, ThermalOK: true, P99MS: 10,
		Unit: "token_like_input_units", UnitScope: performanceUnitScopeTokenLikeInputGeometry,
		MeasuredUnix: uint64(runtimeCellPerformanceNow().Unix()),
	}}
	mustf(t, store.UpsertWorker(ctx, cap), "upsert: %v")
	var tps float32
	var corroborated bool
	if err := pool.QueryRow(ctx,
		`SELECT tps, corroborated FROM worker_tps_cache WHERE worker_id=$1 AND job_type='embed'`,
		workerID,
	).Scan(&tps, &corroborated); err != nil {
		t.Fatal(err)
	}
	if corroborated {
		t.Fatal("self-report alone marked corroborated")
	}
	if tps != 9999 {
		t.Fatalf("unpeered cache tps=%v want claimed 9999 (no peer ⇒ not the floor)", tps)
	}
}

func TestDisputedBenchmarkIsNotRoutableAtClaimedRate(t *testing.T) {
	// Isolated so the only peer is the one this test seeds.
	installBoundCataloguePublicationAuthorityForTest(t)
	ctx, store, pool := openIsolatedTestStore(t)
	// Honest peer at ~100 eps.
	peerSupplier, peerWorker := uuid.New(), uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO suppliers (id,email,status) VALUES ($1,$2,'active')`,
		peerSupplier, "bench-peer-"+uuid.NewString()+"@corp.example"); err != nil {
		t.Fatal(err)
	}
	// Inflated self-report that cannot agree with the peer within 25%.
	supplierID, workerID := uuid.New(), uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO suppliers (id,email,status) VALUES ($1,$2,'active')`,
		supplierID, "bench-"+uuid.NewString()+"@corp.example"); err != nil {
		t.Fatal(err)
	}
	registerContainmentCleanup(t, pool, []uuid.UUID{peerWorker, workerID}, nil)
	peerCap := testWorkerCapability(peerWorker, peerSupplier)
	// Honest peer above the TEST_ONLY floor (~1377) but far from the inflated claim.
	peerCap.Benchmarks = []BenchResult{{
		ModelID: "all-minilm-l6-v2", JobType: "embed",
		EPS: 2000, TPS: 0, ThermalOK: true, P99MS: 10,
		Unit: "token_like_input_units", UnitScope: performanceUnitScopeTokenLikeInputGeometry,
		MeasuredUnix: uint64(runtimeCellPerformanceNow().Unix()),
	}}
	mustf(t, store.UpsertWorker(ctx, peerCap), "peer upsert: %v")

	cap := testWorkerCapability(workerID, supplierID)
	cap.Benchmarks = []BenchResult{{
		ModelID: "all-minilm-l6-v2", JobType: "embed",
		EPS: 9999, TPS: 0, ThermalOK: true, P99MS: 10,
		Unit: "token_like_input_units", UnitScope: performanceUnitScopeTokenLikeInputGeometry,
		MeasuredUnix: uint64(runtimeCellPerformanceNow().Unix()),
	}}
	mustf(t, store.UpsertWorker(ctx, cap), "upsert: %v")
	var tps float32
	var corroborated bool
	if err := pool.QueryRow(ctx,
		`SELECT tps, corroborated FROM worker_tps_cache WHERE worker_id=$1 AND job_type='embed'`,
		workerID,
	).Scan(&tps, &corroborated); err != nil {
		t.Fatal(err)
	}
	if corroborated {
		t.Fatal("disputed self-report marked corroborated")
	}
	if tps != uncorroboratedBenchmarkFloorTPS {
		t.Fatalf("disputed cache tps=%v want floor %v (peer exists and disagrees)",
			tps, uncorroboratedBenchmarkFloorTPS)
	}
}

// ── fixtures ────────────────────────────────────────────────────────────────

func openContainmentTestStore(t *testing.T) (context.Context, *Store, *pgxpool.Pool) {
	t.Helper()
	return openPayoutTestStore(t)
}

// registerContainmentCleanup ages workers offline and cancels residual tasks so
// a later placement/currency claim does not see leftover claimable work. workers
// is ON DELETE RESTRICT from several tables; discarding cleanup errors is not a
// cleanup.
func registerContainmentCleanup(t *testing.T, pool *pgxpool.Pool, workerIDs, jobIDs []uuid.UUID) {
	t.Helper()
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		for _, workerID := range workerIDs {
			if _, err := pool.Exec(c,
				`UPDATE workers SET last_seen_at = now() - interval '10 minutes' WHERE id=$1`,
				workerID); err != nil {
				t.Errorf("age containment worker offline: %v", err)
			}
			if _, err := pool.Exec(c,
				`DELETE FROM worker_authorized_capabilities WHERE worker_id=$1`, workerID); err != nil {
				t.Errorf("cleanup containment capability: %v", err)
			}
			if _, err := pool.Exec(c,
				`DELETE FROM worker_tps_cache WHERE worker_id=$1`, workerID); err != nil {
				t.Errorf("cleanup containment tps cache: %v", err)
			}
		}
		for _, jobID := range jobIDs {
			if _, err := pool.Exec(c,
				`UPDATE tasks SET status='cancelled', claimed_by=NULL
				   WHERE job_id=$1 AND status IN ('queued','retrying','running','verifying')`,
				jobID); err != nil {
				t.Errorf("cancel containment residual tasks: %v", err)
			}
			_, _ = pool.Exec(c, `DELETE FROM claim_independence_exclusions WHERE job_id=$1`, jobID)
			_, _ = pool.Exec(c, `DELETE FROM tasks WHERE job_id=$1`, jobID)
			_, _ = pool.Exec(c, `DELETE FROM jobs WHERE id=$1`, jobID)
		}
	})
}

func testWorkerCapability(workerID, supplierID uuid.UUID) WorkerCapability {
	// Exact physical identity matches installBoundCataloguePublicationAuthorityForTest
	// so enrolment projection can bind when that TEST_ONLY seam is installed.
	// Sandboxed defaults true: ordinary claim eligibility requires containment,
	// and fixtures that need uncontained supply set Sandboxed=false explicitly.
	return WorkerCapability{
		WorkerID:            workerID,
		SupplierID:          supplierID,
		HWClass:             "apple_silicon_pro",
		Engine:              "candle",
		BuildHash:           testOnlyPublicationBuildHash,
		BuildIdentityPolicy: currentEngineBuildIdentityPolicy,
		HardwareIdentity:    testOnlyPublicationHardware,
		MemoryGB:            64,
		MemoryBwGbps:        400,
		GPUCount:            1,
		MemoryGBPerGPU:      64,
		SupportedJobs:       []string{"embed"},
		SupportedModels:     []string{"all-minilm-l6-v2"},
		MinPayoutUsdHr:      0,
		Benchmarks: []BenchResult{{
			ModelID: "all-minilm-l6-v2", JobType: "embed",
			// Above the TEST_ONLY publication conservative floor (~1377).
			EPS: 3000, ThermalOK: true, P99MS: 20,
			Unit: "token_like_input_units", UnitScope: performanceUnitScopeTokenLikeInputGeometry,
			MeasuredUnix: uint64(runtimeCellPerformanceNow().Unix()),
		}},
		AgentVersion: "test",
		OSVersion:    "test",
		Sandboxed:    true,
	}
}
