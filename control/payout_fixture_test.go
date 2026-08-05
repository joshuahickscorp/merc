package main

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// payoutFixture seeds a supplier credit whose cash funding comes from a charged
// buyer collection. Every identifier is unique per call so consecutive suite
// runs against the same database stay green.
type payoutFixture struct {
	actor                        AdminActor
	buyerID, supplierID          uuid.UUID
	jobID, taskID, entryID       uuid.UUID
	paymentIntent, chargeID      string
	creditUSD                    float64
	creditCents, collectionCents int64
	currency                     string
}

type payoutFixtureOpts struct {
	creditUSD        float64
	collectionCents  int64  // 0 → equal to credit floor-cent cash
	jobStatus        string // default "complete"
	payoutStatus     string // default held
	noBuyerCash      bool   // skip charged collection (subsidy-only path)
	verificationFail bool   // default pass
	releaseFuture    bool   // default release is in the past so credit is due
	currency         string // default configured settlement currency
	// supplierID reuses an existing supplier so several credits accrue against
	// one account; zero mints a fresh supplier.
	supplierID uuid.UUID
}

func openPayoutTestStore(t *testing.T) (context.Context, *Store, *pgxpool.Pool) {
	t.Helper()
	databaseURL := requireTestDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	pool, err := pgxpool.New(ctx, databaseURL)
	mustf(t, err, "connect test PostgreSQL: %v")
	t.Cleanup(pool.Close)
	store := NewStore(pool)
	mustf(t, store.Migrate(ctx), "apply canonical schema: %v")
	return ctx, store, pool
}

func seedPayoutFixture(t *testing.T, ctx context.Context, pool *pgxpool.Pool, opts payoutFixtureOpts) payoutFixture {
	t.Helper()
	if opts.creditUSD <= 0 {
		opts.creditUSD = 1.25
	}
	if opts.jobStatus == "" {
		opts.jobStatus = "complete"
	}
	if opts.payoutStatus == "" {
		opts.payoutStatus = PayoutHeld
	}
	if opts.currency == "" {
		opts.currency = SettlementCurrencyCode()
	}
	mustf(t, RequireSettlementCurrency(opts.currency), "fixture currency %q: %v", opts.currency)

	creditMicros := usdToMicros(opts.creditUSD)
	cashCents, _, err := splitSupplierLiabilityMicros(creditMicros)
	mustf(t, err, "split liability: %v")
	if opts.collectionCents == 0 {
		opts.collectionCents = cashCents
	}

	f := payoutFixture{
		actor:           testAdminActor(uuid.New()),
		buyerID:         uuid.New(),
		supplierID:      cmpOrNew(opts.supplierID),
		jobID:           uuid.New(),
		taskID:          uuid.New(),
		entryID:         uuid.New(),
		paymentIntent:   "pi_payout_" + uuid.NewString(),
		chargeID:        "ch_payout_" + uuid.NewString(),
		creditUSD:       opts.creditUSD,
		creditCents:     cashCents,
		collectionCents: opts.collectionCents,
	}

	verdict := "pass"
	if opts.verificationFail {
		verdict = "fail"
	}
	releaseExpr := "now()-interval '1 minute'"
	if opts.releaseFuture {
		releaseExpr = "now()+interval '1 hour'"
	}

	chargeStatus := "not_attempted"
	var chargeReq, chargeRecv, chargeCur, stripePI any
	if !opts.noBuyerCash {
		chargeStatus = "charged"
		chargeReq = opts.collectionCents
		chargeRecv = opts.collectionCents
		chargeCur = opts.currency
		stripePI = f.paymentIntent
	}

	statements := []struct {
		query string
		args  []any
	}{
		{`INSERT INTO api_keys (id,key_hash,is_admin,revoked,name) VALUES ($1,$2,true,false,'payout-admin')`,
			[]any{f.actor.PrincipalID, "payout-admin-" + f.actor.PrincipalID.String()}},
		{`INSERT INTO suppliers (id,email,reputation,status) VALUES ($1,$2,0.5,'active')
		  ON CONFLICT (id) DO NOTHING`,
			[]any{f.supplierID, f.supplierID.String() + "@payout.invalid"}},
		{`INSERT INTO jobs (id,buyer_id,status,job_type,input_ref,charge_status,currency,
		                   stripe_pi,charge_requested_cents,charge_received_cents,charge_currency,terminal_at)
		  VALUES ($1,$2,$3,'embed','payout/input',$4,$5,$6,$7,$8,$9,now())`,
			[]any{f.jobID, f.buyerID, opts.jobStatus, chargeStatus, opts.currency, stripePI, chargeReq, chargeRecv, chargeCur}},
		{`INSERT INTO tasks (id,job_id,status,verification_outcome,completed_at)
		  VALUES ($1,$2,'complete',$3,now())`,
			[]any{f.taskID, f.jobID, verdict}},
		{fmt.Sprintf(`INSERT INTO ledger_entries
		    (id,kind,supplier_id,task_id,amount_usd,currency,payout_status,release_at)
		  VALUES ($1,'supplier_credit',$2,$3,($4::numeric / 1000000),$5,$6,%s)`, releaseExpr),
			[]any{f.entryID, f.supplierID, f.taskID, creditMicros, opts.currency, opts.payoutStatus}},
	}
	for _, statement := range statements {
		if _, err := pool.Exec(ctx, statement.query, statement.args...); err != nil {
			t.Fatalf("seed payout fixture: %v\nSQL: %s", err, statement.query)
		}
	}
	if !opts.noBuyerCash && opts.collectionCents > 0 {
		if _, err := pool.Exec(ctx, `
			INSERT INTO buyer_cash_collections
			  (payment_intent,charge_id,buyer_id,source_kind,job_id,
			   requested_cents,received_cents,currency)
			VALUES ($1,$2,$3,'job',$4,$5,$5,$6)`,
			f.paymentIntent, f.chargeID, f.buyerID, f.jobID, opts.collectionCents, opts.currency); err != nil {
			t.Fatalf("seed buyer cash collection: %v", err)
		}
	}
	f.currency = opts.currency
	return f
}

// seedSiblingCredit adds another held credit on a new task under the same job,
// sharing the job's buyer collection (for oversubscription tests).
func seedSiblingCredit(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	f payoutFixture,
	creditUSD float64,
) (entryID, taskID uuid.UUID, cashCents int64) {
	t.Helper()
	entryID = uuid.New()
	taskID = uuid.New()
	micros := usdToMicros(creditUSD)
	var err error
	cashCents, _, err = splitSupplierLiabilityMicros(micros)
	mustf(t, err, "split sibling liability: %v")
	if _, err := pool.Exec(ctx, `
		INSERT INTO tasks (id,job_id,status,verification_outcome,completed_at)
		VALUES ($1,$2,'complete','pass',now())`, taskID, f.jobID); err != nil {
		t.Fatalf("seed sibling task: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO ledger_entries
		  (id,kind,supplier_id,task_id,amount_usd,currency,payout_status,release_at)
		VALUES ($1,'supplier_credit',$2,$3,($4::numeric / 1000000),$5,'held',now()-interval '1 minute')`,
		entryID, f.supplierID, taskID, micros, f.currency); err != nil {
		t.Fatalf("seed sibling credit: %v", err)
	}
	return entryID, taskID, cashCents
}

// cmpOrNew reuses a caller-supplied supplier so multiple credits land on one
// account, which is what the accrual path needs to be exercised at all.
func cmpOrNew(id uuid.UUID) uuid.UUID {
	if id != uuid.Nil {
		return id
	}
	return uuid.New()
}
