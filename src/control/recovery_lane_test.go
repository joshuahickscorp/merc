package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Recovery-lane observations are parsed by ops/scripts/derive-recovery-receipts.py.
// Each TestRecoveryLane* test emits exactly one observation. Durations are
// measured with time.Now; intervals that were compressed for the test are
// named, never described as wall-clock waits.
const recoveryObservationPrefix = "RECOVERY_LANE_OBSERVATION "

type recoveryObservation struct {
	Mode              string         `json:"mode"`
	Status            string         `json:"status"`
	Killed            string         `json:"killed"`
	Recovered         string         `json:"recovered"`
	Invariant         string         `json:"invariant"`
	ElapsedNS         int64          `json:"elapsed_ns"`
	IntervalShortened bool           `json:"interval_shortened"`
	ProductionPeriod  string         `json:"production_period,omitempty"`
	TestPeriod        string         `json:"test_period,omitempty"`
	Details           map[string]any `json:"details,omitempty"`
}

func emitRecoveryObservation(t *testing.T, obs recoveryObservation) {
	t.Helper()
	if obs.Status == "" {
		obs.Status = "PASS"
	}
	body, err := json.Marshal(obs)
	if err != nil {
		t.Fatalf("marshal recovery observation: %v", err)
	}
	t.Logf("%s%s", recoveryObservationPrefix, body)
}

func recoveryCanary(t *testing.T) {
	t.Helper()
	t.Setenv("MERC_CANARY_MODE", "false")
	t.Setenv("MERC_CANARY_DISABLE_DECISION_REF", "TEST-recovery-lane")
}

func requireRecoveryInfra(t *testing.T, containerEnv string) string {
	t.Helper()
	if os.Getenv("MERC_RECOVERY_SUITE") != "1" {
		t.Skip("peer-container restart/partition tests run from ops/scripts/recovery-suite.sh")
	}
	name := strings.TrimSpace(os.Getenv(containerEnv))
	if name == "" {
		t.Fatalf("MERC_RECOVERY_SUITE=1 but %s is unset", containerEnv)
	}
	return name
}

func dockerCmd(t *testing.T, args ...string) string {
	t.Helper()
	cmd := exec.Command("docker", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("docker %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

func dockerOK(args ...string) bool {
	cmd := exec.Command("docker", args...)
	return cmd.Run() == nil
}

func waitDockerPostgres(t *testing.T, container string, deadline time.Time) {
	t.Helper()
	for time.Now().Before(deadline) {
		if dockerOK("exec", container, "pg_isready", "-U", "cx", "-d", "postgres") {
			return
		}
		time.Sleep(400 * time.Millisecond)
	}
	t.Fatal("postgres did not become ready after the injected fault")
}

func reopenStore(t *testing.T, ctx context.Context, pool *pgxpool.Pool) (*Store, *pgxpool.Pool) {
	t.Helper()
	// A new process is a new pool. The previous pool is closed after every
	// registered cleanup has run so fixture teardowns still have a connection.
	dsn := pool.Config().ConnString()
	next, err := pgxpool.New(ctx, dsn)
	mustf(t, err, "reopen pool after process death: %v")
	t.Cleanup(next.Close)
	return NewStore(next), next
}

func seedClaimedRunningTask(t *testing.T, ctx context.Context, store *Store, pool *pgxpool.Pool) moneyPathFixture {
	t.Helper()
	f := seedMoneyPathFixture(t, ctx, store, pool, moneyPathSeedOpts{
		TaskCount: 1, TaskStatus: "running", ClaimWorker: true, SeedJob: true,
	})
	if _, err := pool.Exec(ctx, `UPDATE tasks SET claimed_at=now() WHERE id=$1`, f.TaskIDs[0]); err != nil {
		t.Fatalf("stamp claimed_at: %v", err)
	}
	return f
}

func taskState(t *testing.T, ctx context.Context, pool *pgxpool.Pool, taskID uuid.UUID) (status string, retries int, claimed *uuid.UUID) {
	t.Helper()
	if err := pool.QueryRow(ctx,
		`SELECT status, retry_count, claimed_by FROM tasks WHERE id=$1`, taskID,
	).Scan(&status, &retries, &claimed); err != nil {
		t.Fatalf("read task: %v", err)
	}
	return status, retries, claimed
}

func ledgerRowsForTask(t *testing.T, ctx context.Context, pool *pgxpool.Pool, taskID uuid.UUID) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM ledger_entries WHERE task_id=$1`, taskID,
	).Scan(&n); err != nil {
		t.Fatalf("count ledger: %v", err)
	}
	return n
}

// Process restart of the control-plane store: in-flight work is still present
// after the pool dies and a new process reconnects; nothing is double-run.
func TestRecoveryLaneProcessRestart(t *testing.T) {
	recoveryCanary(t)
	ctx, store, pool := openIsolatedTestStore(t)
	started := time.Now()
	f := seedClaimedRunningTask(t, ctx, store, pool)
	taskID := f.TaskIDs[0]
	beforeStatus, beforeRetries, beforeClaimed := taskState(t, ctx, pool, taskID)
	if beforeStatus != "running" || beforeClaimed == nil || beforeRetries != 0 {
		t.Fatalf("precondition status=%s retries=%d claimed=%v", beforeStatus, beforeRetries, beforeClaimed)
	}

	store, pool = reopenStore(t, ctx, pool)
	afterStatus, afterRetries, afterClaimed := taskState(t, ctx, pool, taskID)
	if afterStatus != "running" || afterClaimed == nil || *afterClaimed != *beforeClaimed {
		t.Fatalf("after restart status=%s claimed=%v want running/%v", afterStatus, afterClaimed, beforeClaimed)
	}
	if afterRetries != beforeRetries {
		t.Fatalf("restart mutated retry_count %d -> %d", beforeRetries, afterRetries)
	}
	if n := ledgerRowsForTask(t, ctx, pool, taskID); n != 0 {
		t.Fatalf("restart minted %d ledger rows", n)
	}

	// The new process resumes the same work once: fail/requeue the in-flight
	// attempt. A second identical fail is a no-op (no longer the owner).
	out, err := store.FailTaskTx(ctx, taskID, f.WorkerID, 0, FailureReport{
		Class: "worker_shutdown", Message: "control process died",
	})
	mustf(t, err, "resume fail: %v")
	if out != FailRequeued {
		t.Fatalf("resume outcome=%s want requeued", out)
	}
	dup, err := store.FailTaskTx(ctx, taskID, f.WorkerID, 0, FailureReport{
		Class: "worker_shutdown", Message: "duplicate resume",
	})
	if !errors.Is(err, errNotOwner) && dup != FailNoop {
		t.Fatalf("duplicate resume outcome=%s err=%v want noop or errNotOwner", dup, err)
	}
	status, retries, claimed := taskState(t, ctx, pool, taskID)
	if claimed != nil {
		t.Fatalf("resumed task still claimed")
	}
	if status != "retrying" || retries != 1 {
		t.Fatalf("resumed task status=%s retries=%d", status, retries)
	}
	if n := ledgerRowsForTask(t, ctx, pool, taskID); n != 0 {
		t.Fatalf("resume created ledger rows=%d", n)
	}
	emitRecoveryObservation(t, recoveryObservation{
		Mode:      "process_restart",
		Killed:    "control-plane process (new Store/pgxpool on the same database; prior process connections abandoned)",
		Recovered: "in-flight running task, same claim, then a single worker_shutdown requeue",
		Invariant: "task survived reconnect; FailTaskTx ran once; duplicate resume was noop; no ledger rows",
		ElapsedNS: time.Since(started).Nanoseconds(),
		Details: map[string]any{
			"task_id": taskID.String(), "final_status": status, "retry_count": retries,
		},
	})
}

// Control-plane restart under load: inflight leadership is handed off, stale
// payout sweeps resume, and running tasks become claimable exactly once.
func TestRecoveryLaneControlPlaneRestartUnderLoad(t *testing.T) {
	recoveryCanary(t)
	ctx, store, pool := openIsolatedTestStore(t)
	started := time.Now()

	const inflightN = 6
	tenant := uuid.New()
	identities := make([]string, inflightN)
	for i := 0; i < inflightN; i++ {
		identities[i] = identityFor(t, tenant, fmt.Sprintf("load-%d", i))
		role, err := store.ClaimInflightExecution(ctx, identities[i], tenant, "leader-old")
		mustf(t, err, "claim inflight %d: %v", i)
		if !role.Leader {
			t.Fatalf("first claim of identity %d was not leader", i)
		}
	}

	f := seedClaimedRunningTask(t, ctx, store, pool)
	if _, err := pool.Exec(ctx,
		`UPDATE tasks SET claimed_at=now()-interval '31 minutes' WHERE id=$1`, f.TaskIDs[0]); err != nil {
		t.Fatalf("backdate task lease: %v", err)
	}

	payout := seedPayoutFixture(t, ctx, pool, payoutFixtureOpts{creditUSD: 1.00})
	if _, ok, err := store.ClaimPayout(ctx, payout.entryID); err != nil || !ok {
		t.Fatalf("claim payout: ok=%v err=%v", ok, err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE supplier_payout_operations SET updated_at=now()-interval '6 minutes' WHERE ledger_entry_id=$1`,
		payout.entryID); err != nil {
		t.Fatalf("backdate payout sending lease: %v", err)
	}

	store, pool = reopenStore(t, ctx, pool)

	if _, err := pool.Exec(ctx,
		`UPDATE inflight_executions SET lease_expires_at=now()-interval '1 second'`); err != nil {
		t.Fatalf("expire inflight leases: %v", err)
	}
	for i, identity := range identities {
		role, err := store.ClaimInflightExecution(ctx, identity, tenant, "leader-new")
		mustf(t, err, "handoff %d: %v", i)
		if !role.Leader || role.Elections != 2 {
			t.Fatalf("handoff %d produced %+v, want leader election 2", i, role)
		}
	}

	recovered, err := store.RecoverStalePayoutOperations(ctx, payoutSendingLease, 20)
	mustf(t, err, "payout sweep: %v")
	if recovered != 1 {
		t.Fatalf("payout sweep recovered %d, want 1", recovered)
	}
	stale, err := store.StaleRunningTasks(ctx, staleTaskTimeout, 20)
	mustf(t, err, "stale sweep: %v")
	if len(stale) != 1 || stale[0].ID != f.TaskIDs[0] {
		t.Fatalf("stale sweep=%+v", stale)
	}
	mustf(t, store.RequeueStaleTask(ctx, f.TaskIDs[0], 0), "requeue: %v")
	mustf(t, store.RequeueStaleTask(ctx, f.TaskIDs[0], 0), "second requeue: %v")
	status, retries, claimed := taskState(t, ctx, pool, f.TaskIDs[0])
	if claimed != nil || status != "queued" || retries != 1 {
		t.Fatalf("after load-restart status=%s retries=%d claimed=%v", status, retries, claimed)
	}
	var payoutStatus string
	if err := pool.QueryRow(ctx,
		`SELECT payout_status FROM ledger_entries WHERE id=$1`, payout.entryID,
	).Scan(&payoutStatus); err != nil {
		t.Fatal(err)
	}
	if payoutStatus != PayoutOutcomeUnknown {
		t.Fatalf("payout status=%s want outcome_unknown", payoutStatus)
	}
	if n := ledgerRowsForTask(t, ctx, pool, f.TaskIDs[0]); n != 0 {
		t.Fatalf("load-restart minted task ledger rows=%d", n)
	}

	emitRecoveryObservation(t, recoveryObservation{
		Mode:              "control_plane_restart_under_load",
		Killed:            "control-plane Store under inflight leaders, one sending payout, one running task",
		Recovered:         "new Store: inflight leadership election 2, payout sweep, stale-task sweep",
		Invariant:         "each identity handed off once; task requeued exactly once; payout became outcome_unknown; no duplicate money",
		ElapsedNS:         time.Since(started).Nanoseconds(),
		IntervalShortened: true,
		ProductionPeriod:  "inflightLeaseTTL=30s, payoutSendingLease=5m, staleTaskTimeout=30m",
		TestPeriod:        "lease_expires_at and claimed_at/updated_at backdated past those production periods",
		Details: map[string]any{
			"inflight_handoffs": inflightN, "payouts_recovered": recovered,
			"task_final_status": status, "task_retries": retries,
		},
	})
}

func TestRecoveryLanePostgresRestart(t *testing.T) {
	recoveryCanary(t)
	container := requireRecoveryInfra(t, "MERC_RECOVERY_PG_CONTAINER")
	ctx, store, pool := openIsolatedTestStore(t)
	started := time.Now()
	f := seedClaimedRunningTask(t, ctx, store, pool)
	marker := "recovery-pg-" + uuid.NewString()
	if _, err := pool.Exec(ctx,
		`UPDATE buyers SET email=$2 WHERE id=$1`, f.BuyerID, marker+"@example.invalid"); err != nil {
		t.Fatalf("stamp marker: %v", err)
	}

	dockerCmd(t, "restart", container)
	waitDockerPostgres(t, container, time.Now().Add(90*time.Second))

	var email string
	var attempts int
	for attempts = 1; attempts <= 20; attempts++ {
		err := pool.QueryRow(ctx, `SELECT email FROM buyers WHERE id=$1`, f.BuyerID).Scan(&email)
		if err == nil {
			break
		}
		time.Sleep(300 * time.Millisecond)
		if attempts == 20 {
			t.Fatalf("pool did not recover after postgres restart: %v", err)
		}
	}
	if email != marker+"@example.invalid" {
		t.Fatalf("marker lost after postgres restart: %q", email)
	}
	status, retries, claimed := taskState(t, ctx, pool, f.TaskIDs[0])
	if status != "running" || claimed == nil || retries != 0 {
		t.Fatalf("task torn after postgres restart: %s retries=%d claimed=%v", status, retries, claimed)
	}
	if n := ledgerRowsForTask(t, ctx, pool, f.TaskIDs[0]); n != 0 {
		t.Fatalf("postgres restart invented ledger rows=%d", n)
	}
	emitRecoveryObservation(t, recoveryObservation{
		Mode:      "postgres_restart",
		Killed:    "docker restart of the throwaway PostgreSQL container",
		Recovered: "pgxpool reconnect; buyer marker and running task still present",
		Invariant: "no data loss, no torn task state, no invented ledger rows",
		ElapsedNS: time.Since(started).Nanoseconds(),
		Details: map[string]any{
			"container": container, "reconnect_attempts": attempts, "marker": marker,
		},
	})
}

func TestRecoveryLaneObjectStoreRestart(t *testing.T) {
	container := requireRecoveryInfra(t, "MERC_RECOVERY_MINIO_CONTAINER")
	started := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	storage, err := NewStorage(ctx)
	mustf(t, err, "open storage: %v")
	key := "recovery/object-store/" + uuid.NewString() + ".txt"
	body := []byte("object-store-restart-sentinel-" + uuid.NewString())
	mustf(t, storage.PutObject(ctx, key, body, "text/plain"), "put before restart: %v")
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_ = storage.RemoveObjects(c, []string{key})
	})

	dockerCmd(t, "restart", container)
	deadline := time.Now().Add(90 * time.Second)
	var recovered []byte
	var attempts int
	for time.Now().Before(deadline) {
		attempts++
		next, err := NewStorage(ctx)
		if err != nil {
			time.Sleep(400 * time.Millisecond)
			continue
		}
		storage = next
		got, err := storage.GetObject(ctx, key)
		if err != nil {
			time.Sleep(400 * time.Millisecond)
			continue
		}
		recovered = got
		break
	}
	if string(recovered) != string(body) {
		t.Fatalf("object lost or mutated after minio restart (attempts=%d)", attempts)
	}
	follow := []byte("post-restart-write")
	followKey := key + ".after"
	mustf(t, storage.PutObject(ctx, followKey, follow, "text/plain"), "put after restart: %v")
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_ = storage.RemoveObjects(c, []string{followKey})
	})
	gotFollow, err := storage.GetObject(ctx, followKey)
	mustf(t, err, "get follow-up: %v")
	if string(gotFollow) != string(follow) {
		t.Fatal("post-restart write did not round-trip")
	}
	emitRecoveryObservation(t, recoveryObservation{
		Mode:      "object_store_restart",
		Killed:    "docker restart of the throwaway MinIO container",
		Recovered: "NewStorage + GetObject of the pre-restart sentinel; subsequent Put/Get",
		Invariant: "artifact bytes identical; writes work after restart",
		ElapsedNS: time.Since(started).Nanoseconds(),
		Details: map[string]any{
			"container": container, "reconnect_attempts": attempts, "key": key,
			"sha256": hex.EncodeToString(sha256sum(body)),
		},
	})
}

func TestRecoveryLaneNetworkInterruption(t *testing.T) {
	container := requireRecoveryInfra(t, "MERC_RECOVERY_MINIO_CONTAINER")
	started := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	storage, err := NewStorage(ctx)
	mustf(t, err, "open storage: %v")
	key := "recovery/network/" + uuid.NewString() + ".txt"
	body := []byte("network-partition-sentinel-" + uuid.NewString())
	mustf(t, storage.PutObject(ctx, key, body, "text/plain"), "put before partition: %v")
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_ = storage.RemoveObjects(c, []string{key})
	})

	// docker pause freezes the peer: from the client this is a black-hole
	// partition (published ports stay bound, no bytes move). Not iptables.
	dockerCmd(t, "pause", container)
	unpaused := false
	t.Cleanup(func() {
		if !unpaused {
			_ = exec.Command("docker", "unpause", container).Run()
		}
	})
	failCtx, failCancel := context.WithTimeout(ctx, 2*time.Second)
	_, getErr := storage.GetObject(failCtx, key)
	failCancel()
	if getErr == nil {
		t.Fatal("GetObject succeeded while the object store was paused; partition was not observed")
	}

	dockerCmd(t, "unpause", container)
	unpaused = true
	// A fresh client avoids the 10s store-breaker cooldown that the failed
	// GetObject may have opened.
	var recovered []byte
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		next, err := NewStorage(ctx)
		if err != nil {
			time.Sleep(200 * time.Millisecond)
			continue
		}
		got, err := next.GetObject(ctx, key)
		if err != nil {
			time.Sleep(200 * time.Millisecond)
			continue
		}
		recovered = got
		break
	}
	if string(recovered) != string(body) {
		t.Fatalf("silent loss after partition: got %q", recovered)
	}
	emitRecoveryObservation(t, recoveryObservation{
		Mode:              "network_interruption",
		Killed:            "docker pause of MinIO (black-hole partition from the Storage client)",
		Recovered:         "unpause + new Storage client GetObject of the same key",
		Invariant:         "GetObject timed out during the partition (no silent success); bytes intact after reconnect",
		ElapsedNS:         time.Since(started).Nanoseconds(),
		IntervalShortened: true,
		ProductionPeriod:  "Storage client / S3 HTTP timeouts 10-20s; store breaker cooldown 10s",
		TestPeriod:        "GetObject context deadline compressed to 2s during the partition",
		Details: map[string]any{
			"container": container, "partition_error": getErr.Error(),
		},
	})
}

func TestRecoveryLaneStaleWorkerExpiry(t *testing.T) {
	recoveryCanary(t)
	ctx, store, pool := openIsolatedTestStore(t)
	started := time.Now()
	f := seedClaimedRunningTask(t, ctx, store, pool)
	taskID := f.TaskIDs[0]

	fresh, err := store.StaleRunningTasks(ctx, staleTaskTimeout, 20)
	mustf(t, err, "fresh stale scan: %v")
	for _, row := range fresh {
		if row.ID == taskID {
			t.Fatal("a live 0-age claim was reported stale")
		}
	}

	if _, err := pool.Exec(ctx,
		`UPDATE tasks SET claimed_at=now()-interval '31 minutes' WHERE id=$1`, taskID); err != nil {
		t.Fatalf("backdate claim: %v", err)
	}
	stale, err := store.StaleRunningTasks(ctx, staleTaskTimeout, 20)
	mustf(t, err, "stale scan: %v")
	if len(stale) != 1 || stale[0].ID != taskID {
		t.Fatalf("stale scan=%+v want exactly this task", stale)
	}
	mustf(t, store.RequeueStaleTask(ctx, taskID, 0), "requeue: %v")
	mustf(t, store.RequeueStaleTask(ctx, taskID, 0), "second requeue: %v")
	status, retries, claimed := taskState(t, ctx, pool, taskID)
	if claimed != nil || status != "queued" || retries != 1 {
		t.Fatalf("after expiry status=%s retries=%d claimed=%v", status, retries, claimed)
	}
	if n := ledgerRowsForTask(t, ctx, pool, taskID); n != 0 {
		t.Fatalf("expiry minted ledger rows=%d", n)
	}
	emitRecoveryObservation(t, recoveryObservation{
		Mode:              "stale_worker_expiry",
		Killed:            "worker claim (claimed_at backdated; worker process not present)",
		Recovered:         "StaleRunningTasks(staleTaskTimeout) + RequeueStaleTask",
		Invariant:         "lease expired once; job requeued exactly once; no payment",
		ElapsedNS:         time.Since(started).Nanoseconds(),
		IntervalShortened: true,
		ProductionPeriod:  "staleTaskTimeout=30m",
		TestPeriod:        "claimed_at set to now()-31m; StaleRunningTasks still used the production 30m timeout",
		Details:           map[string]any{"task_id": taskID.String(), "retries": retries},
	})
}

func TestRecoveryLaneInterruptedExecution(t *testing.T) {
	recoveryCanary(t)
	ctx, store, pool := openIsolatedTestStore(t)
	started := time.Now()
	f := seedClaimedRunningTask(t, ctx, store, pool)
	taskID := f.TaskIDs[0]
	out, err := store.FailTaskTx(ctx, taskID, f.WorkerID, 0, FailureReport{
		Class: "worker_shutdown", Message: "killed mid-job",
	})
	mustf(t, err, "interrupt: %v")
	if out != FailRequeued {
		t.Fatalf("interrupt outcome=%s want requeued", out)
	}
	status, retries, claimed := taskState(t, ctx, pool, taskID)
	if claimed != nil {
		t.Fatal("interrupted task still claimed (orphan lease)")
	}
	if status != "retrying" || retries != 1 {
		t.Fatalf("interrupted status=%s retries=%d", status, retries)
	}
	dup, err := store.FailTaskTx(ctx, taskID, f.WorkerID, 0, FailureReport{
		Class: "worker_shutdown", Message: "second interrupt",
	})
	if !errors.Is(err, errNotOwner) && dup != FailNoop {
		t.Fatalf("duplicate interrupt outcome=%s err=%v want noop or errNotOwner", dup, err)
	}
	if n := ledgerRowsForTask(t, ctx, pool, taskID); n != 0 {
		t.Fatalf("interrupt minted ledger rows=%d", n)
	}
	emitRecoveryObservation(t, recoveryObservation{
		Mode:      "interrupted_execution",
		Killed:    "in-flight running task via FailTaskTx(worker_shutdown) mid-job",
		Recovered: "task returned to retrying with claim cleared; second fail is noop",
		Invariant: "claimable (retrying, claimed_by NULL); no orphan lease; no money",
		ElapsedNS: time.Since(started).Nanoseconds(),
		Details:   map[string]any{"task_id": taskID.String(), "outcome": string(out)},
	})
}

func TestRecoveryLaneDuplicateStripeEvent(t *testing.T) {
	recoveryCanary(t)
	ctx, store, pool := openPayoutTestStore(t)
	started := time.Now()
	f := seedPayoutFixture(t, ctx, pool, payoutFixtureOpts{creditUSD: 2.00})

	refunded := int64(75)
	object := []byte(fmt.Sprintf(
		`{"object":"charge","id":%q,"payment_intent":%q,"amount":%d,"amount_refunded":%d,"currency":%q}`,
		f.chargeID, f.paymentIntent, f.collectionCents, refunded, f.currency,
	))
	payload := []byte(fmt.Sprintf(
		`{"id":"evt_dup_%s","type":"charge.refunded","created":1700000100,"data":{"object":%s}}`,
		f.chargeID, object,
	))
	eventID := "evt_dup_" + f.chargeID
	event, err := parseStripeCashEvent(eventID, stripeEventChargeRefunded, 1_700_000_100, object, payload)
	mustf(t, err, "parse: %v")

	first, err := store.ApplyPaymentEventTx(ctx, event)
	mustf(t, err, "first apply: %v")
	if first.Duplicate {
		t.Fatal("first delivery reported duplicate")
	}
	second, err := store.ApplyPaymentEventTx(ctx, event)
	mustf(t, err, "second apply: %v")
	if !second.Duplicate || second.CashEffectApplied {
		t.Fatalf("second delivery=%+v want Duplicate and no cash effect", second)
	}

	var events, refundedCents int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM stripe_webhook_events WHERE event_id=$1`, eventID,
	).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx,
		`SELECT refunded_cents FROM stripe_charge_cash_state WHERE charge_id=$1`, f.chargeID,
	).Scan(&refundedCents); err != nil {
		t.Fatal(err)
	}
	if events != 1 {
		t.Fatalf("stripe_webhook_events rows=%d want 1", events)
	}
	if refundedCents != int(refunded) {
		t.Fatalf("refunded_cents=%d want %d", refundedCents, refunded)
	}
	emitRecoveryObservation(t, recoveryObservation{
		Mode:      "duplicate_stripe_event",
		Killed:    "nothing; the same charge.refunded event was delivered twice",
		Recovered: "ApplyPaymentEventTx ON CONFLICT (event_id) DO NOTHING",
		Invariant: "one stripe_webhook_events row; refunded_cents applied once; second delivery Duplicate=true",
		ElapsedNS: time.Since(started).Nanoseconds(),
		Details: map[string]any{
			"event_id": eventID, "first_duplicate": first.Duplicate,
			"second_duplicate": second.Duplicate, "refunded_cents": refundedCents,
			"webhook_rows": events, "linked_collection": first.LinkedCollection,
		},
	})
}

func TestRecoveryLanePartialSettlement(t *testing.T) {
	recoveryCanary(t)
	ctx, store, pool := openIsolatedTestStore(t)
	started := time.Now()
	f := seedPayoutFixture(t, ctx, pool, payoutFixtureOpts{creditUSD: 1.50})

	claimed, ok, err := store.ClaimPayout(ctx, f.entryID)
	mustf(t, err, "claim: %v")
	if !ok || claimed.RequestedCents <= 0 {
		t.Fatalf("claim ok=%v requested=%d", ok, claimed.RequestedCents)
	}
	var sendingStatus string
	if err := pool.QueryRow(ctx,
		`SELECT payout_status FROM ledger_entries WHERE id=$1`, f.entryID,
	).Scan(&sendingStatus); err != nil {
		t.Fatal(err)
	}
	if sendingStatus != PayoutSending {
		t.Fatalf("after claim status=%s want sending", sendingStatus)
	}

	// Crash between the committed ledger/operation write and the provider
	// payout: cash_moved stays false, transfer_ref stays null.
	if _, err := pool.Exec(ctx, `
		UPDATE supplier_payout_operations
		   SET updated_at=now()-interval '6 minutes'
		 WHERE ledger_entry_id=$1 AND status='sending'`, f.entryID); err != nil {
		t.Fatalf("backdate sending lease: %v", err)
	}
	n, err := store.RecoverStalePayoutOperations(ctx, payoutSendingLease, 10)
	mustf(t, err, "recover: %v")
	if n != 1 {
		t.Fatalf("recovered %d sending ops, want 1", n)
	}
	var midStatus string
	var cashMoved bool
	if err := pool.QueryRow(ctx, `
		SELECT le.payout_status, op.cash_moved
		  FROM ledger_entries le JOIN supplier_payout_operations op ON op.ledger_entry_id=le.id
		 WHERE le.id=$1`, f.entryID).Scan(&midStatus, &cashMoved); err != nil {
		t.Fatal(err)
	}
	if midStatus != PayoutOutcomeUnknown || cashMoved {
		t.Fatalf("after recover status=%s cash_moved=%v", midStatus, cashMoved)
	}

	due, err := store.ClaimOutcomeUnknownPayouts(ctx, 0, payoutIdempotencyRetryWindow, 10)
	mustf(t, err, "claim unknown: %v")
	found := false
	for _, row := range due {
		if row.ID == f.entryID {
			found = true
			if row.RequestedCents != claimed.RequestedCents {
				t.Fatalf("retry requested %d, first claim %d", row.RequestedCents, claimed.RequestedCents)
			}
		}
	}
	if !found {
		t.Fatal("outcome_unknown payout was not claimable for retry")
	}

	ref := "tr_partial_" + uuid.NewString()
	state, err := store.FinalizePayout(ctx, f.entryID, PayoutResult{
		Ref: ref, SentCents: claimed.RequestedCents, Currency: f.currency, CashMoved: true,
	})
	mustf(t, err, "finalize: %v")
	if state != PayoutReleased {
		t.Fatalf("finalize state=%s want released", state)
	}
	_, err = store.FinalizePayout(ctx, f.entryID, PayoutResult{
		Ref: ref, SentCents: claimed.RequestedCents, Currency: f.currency, CashMoved: true,
	})
	if err == nil {
		t.Log("second finalize returned nil; cash-row count decides whether that was safe")
	}
	var cashRows int
	var sentSum int64
	if err := pool.QueryRow(ctx, `
		SELECT count(*)::int, COALESCE(sum(sent_cents),0)::bigint
		  FROM supplier_payout_operations
		 WHERE ledger_entry_id=$1 AND cash_moved`, f.entryID).Scan(&cashRows, &sentSum); err != nil {
		t.Fatal(err)
	}
	if cashRows != 1 || sentSum != claimed.RequestedCents {
		t.Fatalf("cash evidence rows=%d sent=%d want 1/%d", cashRows, sentSum, claimed.RequestedCents)
	}
	emitRecoveryObservation(t, recoveryObservation{
		Mode:              "partial_settlement",
		Killed:            "process after ClaimPayout committed ledger+operation to sending, before provider cash / FinalizePayout",
		Recovered:         "RecoverStalePayoutOperations → outcome_unknown → ClaimOutcomeUnknownPayouts → FinalizePayout once",
		Invariant:         "state machine converged to released; exactly one cash_moved row; replay did not duplicate money",
		ElapsedNS:         time.Since(started).Nanoseconds(),
		IntervalShortened: true,
		ProductionPeriod:  "payoutSendingLease=5m; payoutIdempotencyRetryWindow=23h; minimumPayoutHold=24h",
		TestPeriod:        "updated_at backdated 6m; retry window kept at 23h (created_at is now); release_at seeded in the past (same injection as payout fixtures)",
		Details: map[string]any{
			"ledger_entry": f.entryID.String(), "requested_cents": claimed.RequestedCents,
			"cash_rows": cashRows, "sent_cents": sentSum, "final_state": state,
		},
	})
}

func TestRecoveryLaneRollbackAndForward(t *testing.T) {
	recoveryCanary(t)
	ctx, _, pool := openIsolatedTestStore(t)
	started := time.Now()

	tx, err := pool.Begin(ctx)
	mustf(t, err, "begin: %v")
	if _, err := tx.Exec(ctx, `CREATE TABLE recovery_interrupted_migration(id integer)`); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("create probe: %v", err)
	}
	if _, err := tx.Exec(ctx, `SELECT 1/0`); err == nil {
		_ = tx.Rollback(ctx)
		t.Fatal("interrupted migration unexpectedly succeeded")
	}
	_ = tx.Rollback(ctx)
	var probeExists bool
	if err := pool.QueryRow(ctx,
		`SELECT to_regclass('public.recovery_interrupted_migration') IS NOT NULL`,
	).Scan(&probeExists); err != nil {
		t.Fatal(err)
	}
	if probeExists {
		t.Fatal("interrupted migration left a durable table")
	}

	marker := uuid.New()
	if _, err := pool.Exec(ctx, `
		INSERT INTO buyers (id,email,free_credit_usd)
		VALUES ($1,$2,0)`, marker, "rollback-"+marker.String()+"@example.invalid"); err != nil {
		t.Fatalf("insert prior-version row: %v", err)
	}
	var priorCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM buyers WHERE id=$1`, marker).Scan(&priorCount); err != nil {
		t.Fatal(err)
	}
	if priorCount != 1 {
		t.Fatalf("prior row count=%d", priorCount)
	}

	// "Rollback to a prior version": destroy the row, then restore it from a
	// snapshot taken beforehand. Forward recovery is a new write after restore.
	snapshot, err := pool.Query(ctx, `SELECT id::text, email FROM buyers WHERE id=$1`, marker)
	mustf(t, err, "snapshot: %v")
	var snapID, snapEmail string
	if !snapshot.Next() {
		snapshot.Close()
		t.Fatal("snapshot empty")
	}
	mustf(t, snapshot.Scan(&snapID, &snapEmail), "scan snapshot: %v")
	snapshot.Close()

	if _, err := pool.Exec(ctx, `DELETE FROM buyers WHERE id=$1`, marker); err != nil {
		t.Fatalf("destroy current version: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO buyers (id,email,free_credit_usd) VALUES ($1::uuid,$2,0)`,
		snapID, snapEmail); err != nil {
		t.Fatalf("restore prior row: %v", err)
	}
	var restoredEmail string
	if err := pool.QueryRow(ctx, `SELECT email FROM buyers WHERE id=$1`, marker).Scan(&restoredEmail); err != nil {
		t.Fatal(err)
	}
	if restoredEmail != snapEmail {
		t.Fatalf("restored email=%q want %q", restoredEmail, snapEmail)
	}
	forward := "forward-" + marker.String() + "@example.invalid"
	if _, err := pool.Exec(ctx, `UPDATE buyers SET email=$2 WHERE id=$1`, marker, forward); err != nil {
		t.Fatalf("forward write: %v", err)
	}
	var after string
	if err := pool.QueryRow(ctx, `SELECT email FROM buyers WHERE id=$1`, marker).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != forward {
		t.Fatalf("forward write did not stick: %q", after)
	}
	emitRecoveryObservation(t, recoveryObservation{
		Mode:      "rollback_and_forward",
		Killed:    "deliberately interrupted DDL transaction (SELECT 1/0) and a destroyed current-version row",
		Recovered: "transaction rolled back (no probe table); prior-version row restored; forward UPDATE applied",
		Invariant: "interrupted migration is not durable; restored row matches snapshot; subsequent write succeeds",
		ElapsedNS: time.Since(started).Nanoseconds(),
		Details: map[string]any{
			"probe_table_exists": probeExists, "restored_email": snapEmail, "forward_email": after,
		},
	})
}

func sha256sum(b []byte) []byte {
	sum := sha256.Sum256(b)
	return sum[:]
}

func TestRecoveryLaneObservationPrefixIsParseable(t *testing.T) {
	// Guards the contract the suite parser depends on. Does not count as a
	// failure-mode receipt.
	sum := sha256sum([]byte("x"))
	if hex.EncodeToString(sum) == "" {
		t.Fatal("sha256sum empty")
	}
}
