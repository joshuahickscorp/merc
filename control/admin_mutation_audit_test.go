package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
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
		AttributionScope: AdminAttributionNamedOperatorKey, Label: "integration-admin",
	}
}

func TestEveryPrivilegedAdminMutationRequiresReason(t *testing.T) {
	target := uuid.New()
	delta := float32(0.1)
	for _, intent := range []adminMutationIntent{
		{Kind: adminActionWorkerSuspended, TargetKind: adminTargetWorker, TargetID: target, CorrelationRef: "INC-1"},
		{Kind: adminActionWorkerReinstated, TargetKind: adminTargetWorker, TargetID: target, CorrelationRef: "INC-1"},
		{Kind: adminActionTaskRequeued, TargetKind: adminTargetTask, TargetID: target, CorrelationRef: "INC-1"},
		{Kind: adminActionReputationChanged, TargetKind: adminTargetSupplier, TargetID: target, Delta: &delta, CorrelationRef: "INC-1"},
		{Kind: adminActionPayoutReleased, TargetKind: adminTargetLedgerEntry, TargetID: target, CorrelationRef: "INC-1"},
		{Kind: adminActionDisputeResolved, TargetKind: adminTargetDispute, TargetID: target, CorrelationRef: "INC-1", Resolution: "rejected"},
	} {
		if _, err := adminMutationRequestSHA256(intent); !errors.Is(err, errAdminMutationInvalid) {
			t.Fatalf("action %q accepted an empty reason: %v", intent.Kind, err)
		}
	}
}

func TestPrivilegedMutationRequiresIncidentReferenceAndNamedOperator(t *testing.T) {
	intent := adminMutationIntent{Kind: adminActionWorkerSuspended, TargetKind: adminTargetWorker,
		TargetID: uuid.New(), Reason: "containment"}
	if _, err := prepareAdminMutation(testAdminActor(uuid.New()), intent); !errors.Is(err, errAdminMutationInvalid) {
		t.Fatalf("missing incident reference accepted: %v", err)
	}
	unnamed := testAdminActor(uuid.New())
	unnamed.Label = ""
	intent.CorrelationRef = "INC-42"
	if _, err := prepareAdminMutation(unnamed, intent); !errors.Is(err, errAdminActorUnauthorized) {
		t.Fatalf("unnamed operator accepted: %v", err)
	}
}

func TestAdminMutationDigestBindsTargetReasonCorrelationAndDelta(t *testing.T) {
	delta := float32(0.1)
	base := adminMutationIntent{
		Kind: adminActionReputationChanged, TargetKind: adminTargetSupplier,
		TargetID: uuid.New(), Reason: "manual review", CorrelationRef: "request-123", Delta: &delta,
	}
	want, err := adminMutationRequestSHA256(base)
	must(t, err)
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
		mustf(t, err, "%s mutation: %v", name)
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
	must(t, err)
	if body.Reason != " reviewed " || body.RequestID != "incident-42" || body.Delta != float32(0.1) {
		t.Fatalf("decoded privileged mutation body = %+v", body)
	}
}

func openAdminMutationTestStore(t *testing.T) (context.Context, *Store, *pgxpool.Pool) {
	t.Helper()
	databaseURL := requireTestDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	t.Cleanup(cancel)
	pool, err := pgxpool.New(ctx, databaseURL)
	mustf(t, err, "connect disposable PostgreSQL: %v")
	t.Cleanup(pool.Close)
	store := NewStore(pool)
	mustf(t, store.Migrate(ctx), "apply canonical schema: %v")
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

	must(t, store.SuspendWorker(ctx, f.actor, f.workerID, "contain incident", prefix+"-suspend"))
	must(t, store.ReinstateWorker(ctx, f.actor, f.workerID, "review complete", prefix+"-reinstate"))
	must(t, store.AdminForceRequeueTask(ctx, f.actor, f.taskID, "replace execution", prefix+"-requeue"))
	delta := float32(0.1)
	if _, _, err := store.AdminAdjustReputation(ctx, f.actor, f.supplierID, delta, "manual evidence", prefix+"-reputation"); err != nil {
		t.Fatal(err)
	}
	must(t, store.ReleasePayoutTx(ctx, f.actor, f.entryID, "approved liability", prefix+"-payout"))

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
	must(t, err)
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
		must(t, err)
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
	must(t, rows.Err())
	if len(seen) != len(want) {
		t.Fatalf("audited actions=%v, want all %d privileged mutations", seen, len(want))
	}
}

func TestPrivilegedMutationIdempotentConcurrentReplay(t *testing.T) {
	ctx, store, pool := openAdminMutationTestStore(t)
	f := seedAdminMutationFixture(t, ctx, pool)
	correlation := "idempotent-" + uuid.NewString()
	delta := float32(0.1)

	const callers = 8
	errs := make(chan error, callers)
	for range callers {
		go func() {
			before, after, err := store.AdminAdjustReputation(
				ctx, f.actor, f.supplierID, delta, "one reviewed correction", correlation)
			if err == nil && (before != float32(0.5) || after != float32(0.6)) {
				err = fmt.Errorf("replay returned %v -> %v", before, after)
			}
			errs <- err
		}()
	}
	for range callers {
		must(t, <-errs)
	}

	var reputation float32
	var actions int
	must(t, pool.QueryRow(ctx, `SELECT reputation FROM suppliers WHERE id=$1`, f.supplierID).Scan(&reputation))
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM admin_actions WHERE kind=$1 AND correlation_ref=$2`,
		adminActionReputationChanged, correlation).Scan(&actions); err != nil {
		t.Fatal(err)
	}
	if reputation != float32(0.6) || actions != 1 {
		t.Fatalf("idempotent replay produced reputation=%v actions=%d", reputation, actions)
	}

	conflictingDelta := float32(0.2)
	if _, _, err := store.AdminAdjustReputation(ctx, f.actor, f.supplierID, conflictingDelta,
		"one reviewed correction", correlation); !errors.Is(err, errAdminMutationInvalid) {
		t.Fatalf("conflicting correlation reuse returned %v", err)
	}
}

func TestConcurrentNamedOperatorsRetainIndependentAttribution(t *testing.T) {
	ctx, store, pool := openAdminMutationTestStore(t)
	f := seedAdminMutationFixture(t, ctx, pool)
	second := testAdminActor(uuid.New())
	second.Label = "second-integration-admin"
	if _, err := pool.Exec(ctx,
		`INSERT INTO api_keys (id,key_hash,is_admin,revoked,name) VALUES ($1,$2,true,false,$3)`,
		second.PrincipalID, "admin-test-"+second.PrincipalID.String(), second.Label); err != nil {
		t.Fatal(err)
	}

	type operation struct {
		actor AdminActor
		delta float32
		ref   string
	}
	operations := []operation{
		{actor: f.actor, delta: 0.1, ref: "operator-one-" + uuid.NewString()},
		{actor: second, delta: -0.1, ref: "operator-two-" + uuid.NewString()},
	}
	errs := make(chan error, len(operations))
	for _, operation := range operations {
		operation := operation
		go func() {
			_, _, err := store.AdminAdjustReputation(ctx, operation.actor, f.supplierID,
				operation.delta, "independent operator exercise", operation.ref)
			errs <- err
		}()
	}
	for range operations {
		must(t, <-errs)
	}
	for _, operation := range operations {
		var principal uuid.UUID
		var label string
		if err := pool.QueryRow(ctx, `
			SELECT actor_principal_id,actor_label FROM admin_actions
			 WHERE kind=$1 AND correlation_ref=$2`,
			adminActionReputationChanged, operation.ref).Scan(&principal, &label); err != nil {
			t.Fatal(err)
		}
		if principal != operation.actor.PrincipalID || label != operation.actor.Label {
			t.Fatalf("correlation %s attributed to %s/%q", operation.ref, principal, label)
		}
	}
}

func TestRevocationWinsRaceBeforePrivilegedMutation(t *testing.T) {
	ctx, store, pool := openAdminMutationTestStore(t)
	f := seedAdminMutationFixture(t, ctx, pool)
	revokeTx, err := pool.Begin(ctx)
	must(t, err)
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
	must(t, revokeTx.Commit(ctx))
	select {
	case err := <-done:
		if !errors.Is(err, errAdminActorUnauthorized) {
			t.Fatalf("mutation after revocation returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("mutation did not resume after revocation committed")
	}
	var status string
	must(t, pool.QueryRow(ctx, `SELECT status FROM suppliers WHERE id=$1`, f.supplierID).Scan(&status))
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
	must(t, pool.QueryRow(ctx, `SELECT status FROM suppliers WHERE id=$1`, f.supplierID).Scan(&status))
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
