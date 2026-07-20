package main

import (
	"errors"
	"testing"

	"github.com/google/uuid"
)

func TestSupportAndSecurityTechnicalTabletops(t *testing.T) {
	ctx, store, pool := openAdminMutationTestStore(t)
	owner, foreign := uuid.New(), uuid.New()
	jobID, taskID, foreignJobID := uuid.New(), uuid.New(), uuid.New()
	apiKeyID := uuid.New()
	admin := testAdminActor(uuid.New())
	admin.Label = "security-tabletop-operator"
	apiRaw := "cx_test_tabletop_" + uuid.NewString()
	for _, statement := range []struct {
		query string
		args  []any
	}{
		{`INSERT INTO buyers (id,email) VALUES ($1,$2),($3,$4)`,
			[]any{owner, owner.String() + "@tabletop.invalid", foreign, foreign.String() + "@tabletop.invalid"}},
		{`INSERT INTO api_keys (id,buyer_id,key_hash,name) VALUES ($1,$2,$3,'tabletop-token')`,
			[]any{apiKeyID, owner, hashKey(apiRaw)}},
		{`INSERT INTO api_keys (id,key_hash,is_admin,revoked,name) VALUES ($1,$2,true,false,$3)`,
			[]any{admin.PrincipalID, "security-tabletop-admin-" + admin.PrincipalID.String(), admin.Label}},
		{`INSERT INTO jobs (id,buyer_id,status,job_type,input_ref,output_ref)
		  VALUES ($1,$2,'complete','embed','owner/input','owner/output')`, []any{jobID, owner}},
		{`INSERT INTO tasks (id,job_id,status,result_ref,result_key)
		  VALUES ($1,$2,'complete','owner/result','owner/result')`, []any{taskID, jobID}},
		{`INSERT INTO ledger_entries (kind,task_id,amount_usd,payout_status)
		  VALUES ('supplier_credit',$1,1.00,'held'),('buyer_charge',$1,-1.20,'pending'),
		         ('platform_take',$1,0.20,'pending')`, []any{taskID}},
		{`INSERT INTO jobs (id,buyer_id,status,job_type,input_ref,output_ref)
		  VALUES ($1,$2,'complete','embed','foreign/input','foreign/secret-result')`, []any{foreignJobID, foreign}},
	} {
		if _, err := pool.Exec(ctx, statement.query, statement.args...); err != nil {
			t.Fatal(err)
		}
	}

	proposal, err := store.ProposeDisplayedSettlementCorrection(ctx, jobID, 1.10)
	if err != nil {
		t.Fatal(err)
	}
	if !proposal.MismatchDetected || proposal.LedgerUSD != 1.00 || proposal.LedgerMutated ||
		proposal.CommunicationDraft == "" || !proposal.PostmortemRequired {
		t.Fatalf("unsafe or incomplete support proposal: %+v", proposal)
	}
	var ledgerSum float64
	if err := pool.QueryRow(ctx, `SELECT COALESCE(sum(amount_usd),0)::float8 FROM ledger_entries WHERE task_id=$1`,
		taskID).Scan(&ledgerSum); err != nil || ledgerSum != 0 {
		t.Fatalf("support tabletop mutated/broke ledger: sum=%v err=%v", ledgerSum, err)
	}

	if _, err := store.GetJob(ctx, foreignJobID, owner); !errors.Is(err, errNotFound) {
		t.Fatalf("cross-tenant job reference succeeded: %v", err)
	}
	if err := store.CancelJob(ctx, foreignJobID, owner); !errors.Is(err, errNotFound) {
		t.Fatalf("cross-tenant mutation succeeded: %v", err)
	}
	pauseRef := "SEC-TABLETOP-PAUSE-" + uuid.NewString()
	paused, err := store.AdminSetOperationalControl(ctx, admin, controlIntake, true,
		"contain cross-tenant artifact-reference report", pauseRef)
	if err != nil || !paused.Paused {
		t.Fatalf("intake containment did not activate: %+v %v", paused, err)
	}
	revoked, err := store.RevokeAPIKey(ctx, owner, apiKeyID)
	if err != nil || !revoked {
		t.Fatalf("incident token revocation failed: revoked=%v err=%v", revoked, err)
	}
	if _, err := store.LookupAPIKey(ctx, apiRaw); !errors.Is(err, errNotFound) {
		t.Fatalf("revoked incident token still authenticated: %v", err)
	}
	// A leaked artifact reference remains unusable at the application boundary:
	// result presigning begins only after GetJob has verified the buyer owner.
	if _, err := store.GetJob(ctx, foreignJobID, owner); !errors.Is(err, errNotFound) {
		t.Fatalf("cross-tenant artifact precondition succeeded after containment: %v", err)
	}
	// Model object-capability revocation by removing the canonical result
	// pointer before the private object is deleted. No presigned URL was issued
	// to the reporting buyer because the ownership check above failed first.
	if tag, err := pool.Exec(ctx, `UPDATE jobs SET output_ref=NULL WHERE id=$1`, foreignJobID); err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("canonical artifact-reference revocation failed: %v", err)
	}
	var outputRef *string
	if err := pool.QueryRow(ctx, `SELECT output_ref FROM jobs WHERE id=$1`, foreignJobID).Scan(&outputRef); err != nil || outputRef != nil {
		t.Fatalf("revoked artifact reference remained retrievable: %v %v", outputRef, err)
	}
	actions, err := store.ListAdminActions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	foundPauseAudit := false
	for _, action := range actions {
		if action.Kind == adminActionControlChanged && action.CorrelationRef == pauseRef &&
			action.ActorPrincipalID != nil && *action.ActorPrincipalID == admin.PrincipalID &&
			action.ActorLabel == admin.Label && len(action.Detail) > 0 {
			foundPauseAudit = true
		}
	}
	if !foundPauseAudit {
		t.Fatal("containment audit export omitted the attributed intake pause")
	}
	resumed, err := store.AdminSetOperationalControl(ctx, admin, controlIntake, false,
		"ownership denial and revocations verified; resume bounded intake",
		"SEC-TABLETOP-RECOVERY-"+uuid.NewString())
	if err != nil || resumed.Paused {
		t.Fatalf("bounded recovery did not resume intake: %+v %v", resumed, err)
	}
}
