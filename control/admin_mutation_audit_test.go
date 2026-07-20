package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func testAdminActor(id uuid.UUID) AdminActor {
	return AdminActor{
		Mode: AdminAuthBreakGlassAPIKey, PrincipalID: id,
		AttributionScope: AdminAttributionSharedCredentialOnly, Label: "integration-admin",
	}
}

func TestEveryPrivilegedAdminMutationRequiresReason(t *testing.T) {
	target := uuid.New()
	delta := float32(0.1)
	for _, intent := range []adminMutationIntent{
		{Kind: adminActionWorkerSuspended, TargetKind: adminTargetWorker, TargetID: target},
		{Kind: adminActionWorkerReinstated, TargetKind: adminTargetWorker, TargetID: target},
		{Kind: adminActionTaskRequeued, TargetKind: adminTargetTask, TargetID: target},
		{Kind: adminActionReputationChanged, TargetKind: adminTargetSupplier, TargetID: target, Delta: &delta},
		{Kind: adminActionPayoutReleased, TargetKind: adminTargetLedgerEntry, TargetID: target},
	} {
		if _, err := adminMutationRequestSHA256(intent); !errors.Is(err, errAdminMutationInvalid) {
			t.Fatalf("action %q accepted an empty reason: %v", intent.Kind, err)
		}
	}
}

func TestAdminMutationDigestBindsTargetReasonCorrelationAndDelta(t *testing.T) {
	delta := float32(0.1)
	base := adminMutationIntent{
		Kind: adminActionReputationChanged, TargetKind: adminTargetSupplier,
		TargetID: uuid.New(), Reason: "manual review", CorrelationRef: "request-123", Delta: &delta,
	}
	want, err := adminMutationRequestSHA256(base)
	if err != nil {
		t.Fatal(err)
	}
	if again, err := adminMutationRequestSHA256(base); err != nil || again != want {
		t.Fatalf("admin mutation digest is not deterministic: %q %v", again, err)
	}
	changedReason := base
	changedReason.Reason = "different review"
	changedTarget := base
	changedTarget.TargetID = uuid.New()
	changedCorrelation := base
	changedCorrelation.CorrelationRef = "request-124"
	changedDeltaValue := float32(0.2)
	changedDelta := base
	changedDelta.Delta = &changedDeltaValue
	for name, changed := range map[string]adminMutationIntent{
		"reason": changedReason, "target": changedTarget,
		"correlation": changedCorrelation, "delta": changedDelta,
	} {
		got, err := adminMutationRequestSHA256(changed)
		if err != nil {
			t.Fatalf("%s mutation: %v", name, err)
		}
		if got == want {
			t.Fatalf("digest did not bind changed %s", name)
		}
	}
}

func TestAdminActionBodyIsStrictAndBounded(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{"empty", ""},
		{"unknown", `{"reason":"reviewed","unknown":true}`},
		{"duplicate", `{"reason":"one","reason":"two"}`},
		{"trailing", `{"reason":"reviewed"} true`},
		{"oversized", `{"reason":"` + strings.Repeat("x", adminActionBodyLimit) + `"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest("POST", "/", strings.NewReader(tc.body))
			if _, err := decodeAdminActionBody(req); err == nil {
				t.Fatal("invalid privileged mutation body was accepted")
			}
		})
	}
	valid := httptest.NewRequest("POST", "/", strings.NewReader(
		`{"reason":" reviewed ","request_id":"incident-42","delta":0.1}`))
	body, err := decodeAdminActionBody(valid)
	if err != nil {
		t.Fatal(err)
	}
	if body.Reason != " reviewed " || body.RequestID != "incident-42" || body.Delta != float32(0.1) {
		t.Fatalf("decoded privileged mutation body = %+v", body)
	}
}

func openAdminMutationTestStore(t *testing.T) (context.Context, *Store, *pgxpool.Pool) {
	t.Helper()
	databaseURL := os.Getenv("CX_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("CX_TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	t.Cleanup(cancel)
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatalf("connect disposable PostgreSQL: %v", err)
	}
	t.Cleanup(pool.Close)
	store := NewStore(pool)
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("apply canonical schema: %v", err)
	}
	return ctx, store, pool
}

type adminMutationFixture struct {
	actor      AdminActor
	supplierID uuid.UUID
	workerID   uuid.UUID
	jobID      uuid.UUID
	taskID     uuid.UUID
	entryID    uuid.UUID
}

func seedAdminMutationFixture(t *testing.T, ctx context.Context, pool *pgxpool.Pool) adminMutationFixture {
	t.Helper()
	f := adminMutationFixture{
		actor: testAdminActor(uuid.New()), supplierID: uuid.New(), workerID: uuid.New(),
		jobID: uuid.New(), taskID: uuid.New(), entryID: uuid.New(),
	}
	statements := []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO api_keys (id,key_hash,is_admin,revoked,name) VALUES ($1,$2,true,false,'integration-admin')`,
			[]any{f.actor.PrincipalID, "admin-test-" + f.actor.PrincipalID.String()}},
		{`INSERT INTO suppliers (id,email,reputation,status) VALUES ($1,$2,0.5,'active')`,
			[]any{f.supplierID, f.supplierID.String() + "@admin.invalid"}},
		{`INSERT INTO workers (id,supplier_id,hw_class) VALUES ($1,$2,'test')`,
			[]any{f.workerID, f.supplierID}},
		{`INSERT INTO jobs (id,buyer_id,status,job_type,input_ref) VALUES ($1,$2,'running','embed','test/input')`,
			[]any{f.jobID, uuid.New()}},
		{`INSERT INTO tasks (id,job_id,worker_id,claimed_by,status,retry_count) VALUES ($1,$2,$3,$3,'running',0)`,
			[]any{f.taskID, f.jobID, f.workerID}},
		{`INSERT INTO ledger_entries (id,kind,supplier_id,amount_usd,payout_status,release_at)
		  VALUES ($1,'supplier_credit',$2,1.00,'ready',NULL)`, []any{f.entryID, f.supplierID}},
	}
	for _, statement := range statements {
		if _, err := pool.Exec(ctx, statement.sql, statement.args...); err != nil {
			t.Fatalf("seed admin mutation fixture: %v", err)
		}
	}
	return f
}

func TestPrivilegedAdminMutationsHaveCompleteAtomicAudit(t *testing.T) {
	ctx, store, pool := openAdminMutationTestStore(t)
	f := seedAdminMutationFixture(t, ctx, pool)
	prefix := "admin-audit-" + uuid.NewString()

	if err := store.SuspendWorker(ctx, f.actor, f.workerID, "contain incident", prefix+"-suspend"); err != nil {
		t.Fatal(err)
	}
	if err := store.ReinstateWorker(ctx, f.actor, f.workerID, "review complete", prefix+"-reinstate"); err != nil {
		t.Fatal(err)
	}
	if err := store.AdminForceRequeueTask(ctx, f.actor, f.taskID, "replace execution", prefix+"-requeue"); err != nil {
		t.Fatal(err)
	}
	delta := float32(0.1)
	if _, _, err := store.AdminAdjustReputation(ctx, f.actor, f.supplierID, delta, "manual evidence", prefix+"-reputation"); err != nil {
		t.Fatal(err)
	}
	if err := store.ReleasePayoutTx(ctx, f.actor, f.entryID, "approved liability", prefix+"-payout"); err != nil {
		t.Fatal(err)
	}

	want := map[string]adminMutationIntent{
		adminActionWorkerSuspended: {
			Kind: adminActionWorkerSuspended, TargetKind: adminTargetWorker, TargetID: f.workerID,
			Reason: "contain incident", CorrelationRef: prefix + "-suspend",
		},
		adminActionWorkerReinstated: {
			Kind: adminActionWorkerReinstated, TargetKind: adminTargetWorker, TargetID: f.workerID,
			Reason: "review complete", CorrelationRef: prefix + "-reinstate",
		},
		adminActionTaskRequeued: {
			Kind: adminActionTaskRequeued, TargetKind: adminTargetTask, TargetID: f.taskID,
			Reason: "replace execution", CorrelationRef: prefix + "-requeue",
		},
		adminActionReputationChanged: {
			Kind: adminActionReputationChanged, TargetKind: adminTargetSupplier, TargetID: f.supplierID,
			Reason: "manual evidence", CorrelationRef: prefix + "-reputation", Delta: &delta,
		},
		adminActionPayoutReleased: {
			Kind: adminActionPayoutReleased, TargetKind: adminTargetLedgerEntry, TargetID: f.entryID,
			Reason: "approved liability", CorrelationRef: prefix + "-payout",
		},
	}
	rows, err := pool.Query(ctx, `
		SELECT kind,target_kind,target_id,reason,actor_mode,actor_principal_id,
		       attribution_scope,intent_version,request_sha256,correlation_ref,detail
		  FROM admin_actions WHERE actor_principal_id=$1 AND correlation_ref LIKE $2`,
		f.actor.PrincipalID, prefix+"%")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	seen := map[string]bool{}
	for rows.Next() {
		var kind, targetKind, reason, actorMode, scope, digest, correlation string
		var targetID, principalID uuid.UUID
		var version int
		var detail json.RawMessage
		if err := rows.Scan(&kind, &targetKind, &targetID, &reason, &actorMode, &principalID,
			&scope, &version, &digest, &correlation, &detail); err != nil {
			t.Fatal(err)
		}
		expected, ok := want[kind]
		if !ok || seen[kind] {
			t.Fatalf("unexpected or duplicate admin audit action %q", kind)
		}
		seen[kind] = true
		wantDigest, err := adminMutationRequestSHA256(expected)
		if err != nil {
			t.Fatal(err)
		}
		if targetKind != expected.TargetKind || targetID != expected.TargetID || reason != expected.Reason ||
			actorMode != string(f.actor.Mode) || principalID != f.actor.PrincipalID ||
			scope != string(f.actor.AttributionScope) || version != adminMutationIntentVersion ||
			digest != wantDigest || correlation != expected.CorrelationRef {
			t.Fatalf("incomplete audit row for %s", kind)
		}
		var states struct {
			Before map[string]any `json:"before"`
			After  map[string]any `json:"after"`
		}
		if err := json.Unmarshal(detail, &states); err != nil || len(states.Before) == 0 || len(states.After) == 0 {
			t.Fatalf("audit %s lacks before/after state: %s (%v)", kind, detail, err)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(seen) != len(want) {
		t.Fatalf("audited actions=%v, want all %d privileged mutations", seen, len(want))
	}
}

func TestRevocationWinsRaceBeforePrivilegedMutation(t *testing.T) {
	ctx, store, pool := openAdminMutationTestStore(t)
	f := seedAdminMutationFixture(t, ctx, pool)
	revokeTx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer revokeTx.Rollback(ctx)
	if _, err := revokeTx.Exec(ctx, `UPDATE api_keys SET revoked=true WHERE id=$1`, f.actor.PrincipalID); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		done <- store.SuspendWorker(ctx, f.actor, f.workerID, "race test", "revoke-race-"+uuid.NewString())
	}()
	select {
	case err := <-done:
		t.Fatalf("mutation bypassed the uncommitted revocation lock: %v", err)
	case <-time.After(150 * time.Millisecond):
	}
	if err := revokeTx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, errAdminActorUnauthorized) {
			t.Fatalf("mutation after revocation returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("mutation did not resume after revocation committed")
	}
	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM suppliers WHERE id=$1`, f.supplierID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "active" {
		t.Fatalf("revoked actor changed supplier status to %q", status)
	}
	var actions int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM admin_actions WHERE actor_principal_id=$1`, f.actor.PrincipalID).Scan(&actions); err != nil {
		t.Fatal(err)
	}
	if actions != 0 {
		t.Fatalf("revoked mutation wrote %d audit actions", actions)
	}
}

func TestAdminMutationRollsBackWhenAuditInsertFails(t *testing.T) {
	ctx, store, pool := openAdminMutationTestStore(t)
	f := seedAdminMutationFixture(t, ctx, pool)
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")
	functionName := "cx_test_fail_admin_audit_" + suffix
	triggerName := "cx_test_fail_admin_audit_trigger_" + suffix
	ddl := fmt.Sprintf(`
		CREATE FUNCTION %s() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
		  IF NEW.target_id = '%s'::uuid THEN RAISE EXCEPTION 'forced admin audit failure'; END IF;
		  RETURN NEW;
		END $$;
		CREATE TRIGGER %s BEFORE INSERT ON admin_actions
		FOR EACH ROW EXECUTE FUNCTION %s()`, functionName, f.workerID, triggerName, functionName)
	if _, err := pool.Exec(ctx, ddl); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = pool.Exec(cleanupCtx, fmt.Sprintf(
			"DROP TRIGGER IF EXISTS %s ON admin_actions; DROP FUNCTION IF EXISTS %s()", triggerName, functionName))
	})

	err := store.SuspendWorker(ctx, f.actor, f.workerID, "must roll back", "rollback-"+uuid.NewString())
	if err == nil || errors.Is(err, errAdminActorUnauthorized) || errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("forced audit failure returned %v", err)
	}
	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM suppliers WHERE id=$1`, f.supplierID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "active" {
		t.Fatalf("audit insert failure left mutation committed: supplier status=%q", status)
	}
	var actions int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM admin_actions WHERE target_id=$1`, f.workerID).Scan(&actions); err != nil {
		t.Fatal(err)
	}
	if actions != 0 {
		t.Fatalf("failed transaction left %d audit rows", actions)
	}
}
