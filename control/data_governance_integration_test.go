package main

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestDSARDeletionTombstoneAndRestoreReplay(t *testing.T) {
	ctx, store, pool := openAdminMutationTestStore(t)
	buyerID, foreignBuyerID := uuid.New(), uuid.New()
	jobID, foreignJobID, taskID := uuid.New(), uuid.New(), uuid.New()
	supplierID, workerID := uuid.New(), uuid.New()
	email := "subject-" + buyerID.String() + "@example.invalid"
	foreignEmail := "foreign-" + foreignBuyerID.String() + "@example.invalid"
	admin := testAdminActor(uuid.New())
	admin.Label = "privacy-exercise-operator"
	suffix := strings.ReplaceAll(buyerID.String(), "-", "")
	sessionRaw := "cx_sess_subject_secret_" + suffix
	apiRaw := "cx_test_subject_secret_" + suffix
	paymentIntent := "pi_test_dsar_" + suffix
	chargeID := "ch_test_dsar_" + suffix
	customerID := "cus_test_dsar_" + suffix
	paymentMethod := "pm_test_dsar_" + suffix
	inputRef := "private/" + buyerID.String() + "/input.jsonl"
	outputRef := "private/" + buyerID.String() + "/result.jsonl"
	taskRef := "private/" + buyerID.String() + "/task-result.jsonl"

	statements := []struct {
		query string
		args  []any
	}{
		{`INSERT INTO api_keys (id,key_hash,is_admin,revoked,name) VALUES ($1,$2,true,false,$3)`,
			[]any{admin.PrincipalID, "admin-test-" + admin.PrincipalID.String(), admin.Label}},
		{`INSERT INTO buyers (id,email,password_hash,free_credit_usd) VALUES ($1,$2,'synthetic-password-hash',5)`,
			[]any{buyerID, email}},
		{`INSERT INTO buyers (id,email,password_hash,free_credit_usd) VALUES ($1,$2,'foreign-password-hash',7)`,
			[]any{foreignBuyerID, foreignEmail}},
		{`INSERT INTO sessions (token_hash,buyer_id,expires_at) VALUES ($1,$2,now()+interval '1 day')`,
			[]any{hashKey(sessionRaw), buyerID}},
		{`INSERT INTO api_keys (buyer_id,key_hash,name,masked) VALUES ($1,$2,'subject-key','cx_test_...cret')`,
			[]any{buyerID, hashKey(apiRaw)}},
		{`INSERT INTO suppliers (id,email,status) VALUES ($1,$2,'active')`,
			[]any{supplierID, supplierID.String() + "@supplier.invalid"}},
		{`INSERT INTO workers (id,supplier_id,hw_class,engine,build_hash,version)
		  VALUES ($1,$2,'apple_silicon_max','candle','synthetic-build','0.1.0')`, []any{workerID, supplierID}},
		{`INSERT INTO jobs (id,buyer_id,status,job_type,model_ref,input_ref,output_ref,tier,task_count,tasks_done,
		  charge_status,stripe_pi,billed_usd,submit_idempotency_key,submit_request_sha256)
		  VALUES ($1,$2,'complete','embed','model-revision',$3,$4,'batch',1,1,
		  'charged',$5,1.25,'dsar-submit-key',$6)`,
			[]any{jobID, buyerID, inputRef, outputRef, paymentIntent, strings.Repeat("b", 64)}},
		{`INSERT INTO jobs (id,buyer_id,status,job_type,input_ref) VALUES ($1,$2,'complete','embed','foreign/private')`,
			[]any{foreignJobID, foreignBuyerID}},
		{`INSERT INTO tasks (id,job_id,status,input_ref,result_ref,result_key,result_sha256,chunk_index,
		  execution_worker_id,execution_supplier_id,execution_hw_class,execution_engine,execution_build_hash,
		  verification_outcome,verified_at)
		  VALUES ($1,$2,'complete',$3,$4,$4,$5,0,$6,$7,'apple_silicon_max','candle','synthetic-build','pass',now())`,
			[]any{taskID, jobID, inputRef, taskRef, strings.Repeat("a", 64), workerID, supplierID}},
		{`INSERT INTO task_execution_history (task_id,attempt,worker_id,supplier_id)
		  VALUES ($1,0,$2,$3)`, []any{taskID, workerID, supplierID}},
		{`INSERT INTO task_verdicts
		  (task_id,attempt,job_id,supplier_id,outcome,result_sha256,decision_version,decision_sha256,
		   artifact_key,artifact_sha256)
		  VALUES ($1,0,$2,$3,'pass',$4,1,$5,$6,$4)`,
			[]any{taskID, jobID, supplierID, strings.Repeat("a", 64), strings.Repeat("c", 64), taskRef}},
		{`INSERT INTO ledger_entries (kind,buyer_id,task_id,amount_usd,payout_status,payout_ref)
		  VALUES ('buyer_charge',$1,$2,-1.25,'pending','pi_test_dsar')`, []any{buyerID, taskID}},
		{`INSERT INTO billing_customers (buyer_id,stripe_customer_id,default_payment_method)
		  VALUES ($1,$2,$3)`, []any{buyerID, customerID, paymentMethod}},
		{`INSERT INTO buyer_charge_operations
		  (operation_key,source_kind,job_id,buyer_id,stripe_customer,stripe_payment_method,amount_cents,
		   currency,status,payment_intent,charge_id)
		  VALUES ('dsar-op-'||$1::text,'job',$2,$1::uuid,$3,$4,125,'usd',
		          'succeeded',$5,$6)`, []any{buyerID, jobID, customerID, paymentMethod, paymentIntent, chargeID}},
		{`INSERT INTO buyer_cash_collections
		  (payment_intent,charge_id,buyer_id,source_kind,job_id,requested_cents,received_cents,currency)
		  VALUES ($3,$4,$1,'job',$2,125,125,'usd')`, []any{buyerID, jobID, paymentIntent, chargeID}},
		{`INSERT INTO webhooks (buyer_id,job_id,url,signing_secret_sealed)
		  VALUES ($1,$2,'https://subject.example.invalid/hook','enc:never-export-this')`, []any{buyerID, jobID}},
		{`INSERT INTO job_events (job_id,task_id,event,buyer_text,detail)
		  VALUES ($1,$2,'finalized','completed','{"source":"synthetic"}')`, []any{jobID, taskID}},
		{`INSERT INTO quotes (buyer_id,job_type,model_ref,quote_json)
		  VALUES ($1,'embed','model-revision','{"private":"quote"}')`, []any{buyerID}},
	}
	for _, statement := range statements {
		if _, err := pool.Exec(ctx, statement.query, statement.args...); err != nil {
			t.Fatalf("seed data-governance fixture: %v\n%s", err, statement.query)
		}
	}

	export, err := store.ExportBuyerData(ctx, buyerID)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(export)
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	for _, required := range []string{email, jobID.String(), taskID.String(), workerID.String(), inputRef, outputRef,
		paymentIntent, chargeID, "model-revision", "dsar-submit-key", "private", "finalized", "pass"} {
		if !strings.Contains(text, required) {
			t.Fatalf("DSAR export omits %q", required)
		}
	}
	for _, forbidden := range []string{foreignEmail, foreignJobID.String(), hashKey(sessionRaw),
		hashKey(apiRaw), "synthetic-password-hash", "never-export-this"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("DSAR export leaked foreign or secret value category for %q", forbidden)
		}
	}

	correlation := "PRIV-DSAR-" + uuid.NewString()
	result, err := store.TombstoneBuyer(ctx, admin, buyerID, "verified deletion request", correlation)
	if err != nil {
		t.Fatal(err)
	}
	if result.BuyerID != buyerID || result.TombstoneID == uuid.Nil || !result.FinancialRetained {
		t.Fatalf("incomplete deletion result: %+v", result)
	}
	artifactSet := map[string]bool{}
	for _, ref := range result.ArtifactRefs {
		artifactSet[ref] = true
	}
	for _, ref := range []string{inputRef, outputRef, taskRef} {
		if !artifactSet[ref] {
			t.Fatalf("artifact deletion manifest omitted %q", ref)
		}
		delete(artifactSet, ref) // mock the external object deletion exercised by the caller
	}
	if len(artifactSet) != 0 {
		t.Fatalf("unexpected artifact refs remain in deletion mock: %v", artifactSet)
	}
	if _, err := store.LookupSession(ctx, sessionRaw); !errors.Is(err, errNotFound) {
		t.Fatalf("deleted session authenticated: %v", err)
	}
	if _, err := store.LookupAPIKey(ctx, apiRaw); !errors.Is(err, errNotFound) {
		t.Fatalf("deleted API key authenticated: %v", err)
	}
	if _, err := store.ExportBuyerData(ctx, buyerID); !errors.Is(err, errNotFound) {
		t.Fatalf("deleted account still exported private data: %v", err)
	}
	if _, err := store.CreateBuyerAccount(ctx, email, "replacement-password", 0); !errors.Is(err, errEmailTaken) {
		t.Fatalf("tombstoned identity was silently recreated: %v", err)
	}
	if replay, err := store.TombstoneBuyer(ctx, admin, buyerID, "verified deletion request", correlation); err != nil ||
		replay.TombstoneID != result.TombstoneID {
		t.Fatalf("tombstone retry was not idempotent: %+v %v", replay, err)
	}

	var password *string
	var deletedAt *time.Time
	var currentEmail, restoredInput string
	var financialRows, webhookRows, quoteRows int
	if err := pool.QueryRow(ctx, `SELECT email,password_hash,deleted_at FROM buyers WHERE id=$1`, buyerID).Scan(
		&currentEmail, &password, &deletedAt); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(currentEmail, "deleted+") || password != nil || deletedAt == nil {
		t.Fatalf("buyer row not minimally tombstoned: %q %v %v", currentEmail, password, deletedAt)
	}
	if err := pool.QueryRow(ctx, `SELECT input_ref FROM jobs WHERE id=$1`, jobID).Scan(&restoredInput); err != nil {
		t.Fatal(err)
	}
	if restoredInput == inputRef {
		t.Fatal("private job input reference survived deletion")
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM buyer_cash_collections WHERE buyer_id=$1`, buyerID).Scan(&financialRows); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM webhooks WHERE buyer_id=$1`, buyerID).Scan(&webhookRows); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM quotes WHERE buyer_id=$1`, buyerID).Scan(&quoteRows); err != nil {
		t.Fatal(err)
	}
	if financialRows != 1 || webhookRows != 0 || quoteRows != 0 {
		t.Fatalf("retention/removal mismatch financial=%d webhooks=%d quotes=%d", financialRows, webhookRows, quoteRows)
	}
	var stillForeign string
	if err := pool.QueryRow(ctx, `SELECT email FROM buyers WHERE id=$1`, foreignBuyerID).Scan(&stillForeign); err != nil || stillForeign != foreignEmail {
		t.Fatalf("foreign buyer changed: %q %v", stillForeign, err)
	}

	// Model a pre-deletion backup restore while retaining the independent
	// tombstone journal, then prove replay removes the resurrected data again.
	if _, err := pool.Exec(ctx, `UPDATE buyers SET email=$2,password_hash='restored-secret',deleted_at=NULL,
		deletion_tombstone_id=NULL WHERE id=$1`, buyerID, email); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE jobs SET input_ref=$2 WHERE id=$1`, jobID, inputRef); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO sessions (token_hash,buyer_id,expires_at)
		VALUES ($1,$2,now()+interval '1 day')`, hashKey(sessionRaw), buyerID); err != nil {
		t.Fatal(err)
	}
	count, err := store.ApplyBuyerTombstones(ctx)
	if err != nil || count < 1 {
		t.Fatalf("restore tombstone replay: count=%d err=%v", count, err)
	}
	if _, err := store.LookupSession(ctx, sessionRaw); !errors.Is(err, errNotFound) {
		t.Fatalf("tombstone replay left restored authentication active: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT email,password_hash,deleted_at FROM buyers WHERE id=$1`, buyerID).Scan(
		&currentEmail, &password, &deletedAt); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(currentEmail, "deleted+") || password != nil || deletedAt == nil {
		t.Fatalf("restore replay failed to re-tombstone buyer: %q %v %v", currentEmail, password, deletedAt)
	}
	if _, err := pool.Exec(ctx, `UPDATE buyer_identity_tombstones SET reason='tampered' WHERE id=$1`,
		result.TombstoneID); err == nil {
		t.Fatal("append-only tombstone accepted mutation")
	}
}
