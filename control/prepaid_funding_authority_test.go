package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// insertTestAdminActor mints a break-glass operator principal that
// revalidateAdminActor can still resolve inside the refund transaction.
func insertTestAdminActor(t *testing.T, pool *pgxpool.Pool, ctx context.Context) AdminActor {
	t.Helper()
	actor := testAdminActor(uuid.New())
	if _, err := pool.Exec(ctx, `
		INSERT INTO api_keys (id,key_hash,is_admin,revoked,name)
		VALUES ($1,$2,true,false,'prepaid-refund-operator')`,
		actor.PrincipalID, "prepaid-admin-"+actor.PrincipalID.String()); err != nil {
		t.Fatalf("insert admin principal: %v", err)
	}
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = pool.Exec(c, `DELETE FROM admin_actions WHERE actor_principal_id=$1`, actor.PrincipalID)
		_, _ = pool.Exec(c, `DELETE FROM api_keys WHERE id=$1`, actor.PrincipalID)
	})
	return actor
}

// fundPrepaidViaTopup credits the buyer the way production does: a collected
// payment intent, so refunds have a real intent to trace back to.
func fundPrepaidViaTopup(t *testing.T, ctx context.Context, store *Store, buyerID uuid.UUID, cents int64) string {
	t.Helper()
	opKey := "topup-seed-" + uuid.NewString()
	if _, err := store.BeginPrepaidTopup(ctx, opKey, buyerID, cents); err != nil {
		t.Fatalf("arm top-up: %v", err)
	}
	intentID := "pi_seed_" + uuid.NewString()
	if err := store.CreditPrepaidTopup(ctx, opKey, buyerID, ChargeResult{
		PaymentIntentID: intentID, ChargeID: "ch_seed_" + uuid.NewString(),
		RequestedCents: cents, ReceivedCents: cents, Currency: "usd",
	}); err != nil {
		t.Fatalf("credit top-up: %v", err)
	}
	return intentID
}

type fundingHarness struct {
	store     *Store
	pool      *pgxpool.Pool
	ctx       context.Context
	handler   http.Handler
	charges   *atomic.Int64
	refunds   *atomic.Int64
	refundErr *atomic.Bool
}

// newFundingHarness wires the real route table to a fake Stripe so the tests
// assert on the HTTP surface a buyer actually reaches, not on handler internals.
func newFundingHarness(t *testing.T) *fundingHarness {
	t.Helper()
	store, pool, ctx := prepaidTestStore(t)
	var charges, refunds atomic.Int64
	var refundErr atomic.Bool
	withStripeTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/payment_intents"):
			_ = r.ParseForm()
			charges.Add(1)
			amount := r.Form.Get("amount")
			fmt.Fprintf(w, `{"id":"pi_live_%d","latest_charge":{"id":"ch_live_%d"},`+
				`"status":"succeeded","currency":"usd","amount":%s,"amount_received":%s}`,
				charges.Load(), charges.Load(), amount, amount)
		case strings.HasSuffix(r.URL.Path, "/refunds"):
			if refundErr.Load() {
				w.WriteHeader(http.StatusInternalServerError)
				fmt.Fprint(w, `{"error":{"type":"api_error","message":"stripe is down"}}`)
				return
			}
			refunds.Add(1)
			fmt.Fprintf(w, `{"id":"re_live_%d","status":"succeeded"}`, refunds.Load())
		default:
			fmt.Fprint(w, `{"id":"obj_test"}`)
		}
	}))
	return &fundingHarness{
		store: store, pool: pool, ctx: ctx,
		handler: NewServer(store, nil, nil, nil).Routes(),
		charges: &charges, refunds: &refunds, refundErr: &refundErr,
	}
}

// buyerWithCard returns a buyer holding a saved payment method plus the raw API
// key that authenticates as them.
func (h *fundingHarness) buyerWithCard(t *testing.T) (uuid.UUID, string) {
	t.Helper()
	buyerID := insertTestBuyer(t, h.pool, h.ctx)
	cust := "cus_" + buyerID.String()
	if err := h.store.UpsertBillingCustomer(h.ctx, buyerID, cust); err != nil {
		t.Fatal(err)
	}
	if err := h.store.SetBillingPMByCustomer(h.ctx, cust, "pm_"+buyerID.String()); err != nil {
		t.Fatal(err)
	}
	raw := "cx_test_buyer_" + uuid.NewString()
	if _, err := h.pool.Exec(h.ctx, `
		INSERT INTO api_keys (buyer_id,key_hash,is_admin,revoked,name)
		VALUES ($1,$2,false,false,'funding-test')`, buyerID, hashKey(raw)); err != nil {
		t.Fatal(err)
	}
	return buyerID, raw
}

func (h *fundingHarness) topup(t *testing.T, key, idem, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/billing/topup", strings.NewReader(body))
	req.RemoteAddr = "127.0.0.1:11111"
	req.Header.Set("Authorization", "Bearer "+key)
	if idem != "" {
		req.Header.Set("Idempotency-Key", idem)
	}
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	return rec
}

func decodeBody(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode %s: %v", rec.Body.String(), err)
	}
	return out
}

// TestBuyerFundsOwnAccountThroughRegisteredRoute is the S3 headline: before the
// route existed, POST /v1/billing/topup 404ed and no buyer could put money in,
// while job admission told them to call exactly that path.
func TestBuyerFundsOwnAccountThroughRegisteredRoute(t *testing.T) {
	h := newFundingHarness(t)
	buyerID, key := h.buyerWithCard(t)

	rec := h.topup(t, key, "first-deposit", `{"amount_usd":25}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("top-up status %d body %s, want 200", rec.Code, rec.Body.String())
	}
	bal, err := h.store.BuyerPrepaidBalanceMicros(h.ctx, buyerID)
	if err != nil {
		t.Fatal(err)
	}
	if bal != 25_000_000 {
		t.Fatalf("balance after $25 top-up = %d micro-USD, want 25000000", bal)
	}
	if got := h.charges.Load(); got != 1 {
		t.Fatalf("stripe charges = %d, want 1", got)
	}
}

// TestTopupRefusesFabricatedIdempotency pins defect (b): the handler used to
// mint a fresh key per request, so a lost response invited the buyer to charge
// their card a second time for the same deposit.
func TestTopupRefusesFabricatedIdempotency(t *testing.T) {
	h := newFundingHarness(t)
	_, key := h.buyerWithCard(t)

	rec := h.topup(t, key, "", `{"amount_usd":25}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("top-up without Idempotency-Key: status %d body %s, want 400",
			rec.Code, rec.Body.String())
	}
	if got := h.charges.Load(); got != 0 {
		t.Fatalf("an unkeyed top-up reached Stripe %d times, want 0", got)
	}
}

func TestTopupRetryDoesNotDuplicateValue(t *testing.T) {
	h := newFundingHarness(t)
	buyerID, key := h.buyerWithCard(t)

	for attempt := 0; attempt < 3; attempt++ {
		rec := h.topup(t, key, "deposit-42", `{"amount_usd":25}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("attempt %d: status %d body %s, want 200", attempt, rec.Code, rec.Body.String())
		}
	}
	bal, _ := h.store.BuyerPrepaidBalanceMicros(h.ctx, buyerID)
	if bal != 25_000_000 {
		t.Fatalf("balance after three identical top-ups = %d, want 25000000", bal)
	}
	if got := h.charges.Load(); got != 1 {
		t.Fatalf("stripe charges = %d, want 1 (retries must be answered from the operation row)", got)
	}
}

func TestTopupRefusesConflictingIdempotencyKey(t *testing.T) {
	h := newFundingHarness(t)
	buyerID, key := h.buyerWithCard(t)

	if rec := h.topup(t, key, "deposit-42", `{"amount_usd":25}`); rec.Code != http.StatusOK {
		t.Fatalf("first top-up: status %d body %s", rec.Code, rec.Body.String())
	}
	rec := h.topup(t, key, "deposit-42", `{"amount_usd":250}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("reused key with a different amount: status %d body %s, want 409",
			rec.Code, rec.Body.String())
	}
	bal, _ := h.store.BuyerPrepaidBalanceMicros(h.ctx, buyerID)
	if bal != 25_000_000 {
		t.Fatalf("balance after refused conflict = %d, want 25000000", bal)
	}
	if got := h.charges.Load(); got != 1 {
		t.Fatalf("stripe charges = %d, want 1", got)
	}
}

// TestTopupCreditsOnlyTheAuthenticatedBuyer covers both halves of "a buyer
// cannot fund or inspect another's account": the credited buyer comes from the
// credential, and two buyers picking the same Idempotency-Key do not collide on
// one globally-keyed operation row.
func TestTopupCreditsOnlyTheAuthenticatedBuyer(t *testing.T) {
	h := newFundingHarness(t)
	firstID, firstKey := h.buyerWithCard(t)
	secondID, secondKey := h.buyerWithCard(t)

	for _, credential := range []string{firstKey, secondKey} {
		if rec := h.topup(t, credential, "shared-key", `{"amount_usd":25}`); rec.Code != http.StatusOK {
			t.Fatalf("top-up: status %d body %s, want 200", rec.Code, rec.Body.String())
		}
	}
	for _, buyerID := range []uuid.UUID{firstID, secondID} {
		bal, _ := h.store.BuyerPrepaidBalanceMicros(h.ctx, buyerID)
		if bal != 25_000_000 {
			t.Fatalf("buyer %s balance = %d, want 25000000", buyerID, bal)
		}
	}

	// No buyer-supplied field can redirect the credit: the second buyer's own
	// balance moves, never the first's.
	rec := h.topup(t, secondKey, "targeted", `{"amount_usd":25,"buyer_id":"`+firstID.String()+`"}`)
	if rec.Code == http.StatusOK {
		t.Fatalf("top-up accepted an unknown buyer_id field: %s", rec.Body.String())
	}
	bal, _ := h.store.BuyerPrepaidBalanceMicros(h.ctx, firstID)
	if bal != 25_000_000 {
		t.Fatalf("first buyer balance moved to %d on another buyer's request", bal)
	}
}

// TestPrepaidRefundIsOperatorAuthorityNotBuyerSelfService pins the authority
// boundary: the raw remainder refund is cash leaving the platform over card
// rails, so it lives behind authAdmin and there is no buyer-reachable route.
func TestPrepaidRefundIsOperatorAuthorityNotBuyerSelfService(t *testing.T) {
	h := newFundingHarness(t)
	buyerID, key := h.buyerWithCard(t)

	req := httptest.NewRequest(http.MethodPost, "/v1/billing/refund", strings.NewReader(`{}`))
	req.RemoteAddr = "127.0.0.1:11111"
	req.Header.Set("Authorization", "Bearer "+key)
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("buyer-facing refund route: status %d, want 404 (refund is operator authority)", rec.Code)
	}

	// The operator route rejects a buyer credential outright.
	req = httptest.NewRequest(http.MethodPost,
		"/admin/buyers/"+buyerID.String()+"/prepaid-refund",
		strings.NewReader(`{"reason":"buyer closed account","request_id":"INC-1"}`))
	req.RemoteAddr = "127.0.0.1:11111"
	req.Header.Set("Authorization", "Bearer "+key)
	rec = httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("buyer credential on the operator refund route: status %d, want 401", rec.Code)
	}
}

func TestPrepaidRefundRequiresReasonAndCorrelationReference(t *testing.T) {
	h := newFundingHarness(t)
	buyerID := insertTestBuyer(t, h.pool, h.ctx)
	actor := insertTestAdminActor(t, h.pool, h.ctx)
	fundPrepaidViaTopup(t, h.ctx, h.store, buyerID, 2500)

	server := &Server{store: h.store}
	for name, ref := range map[string]struct{ reason, correlation string }{
		"no reason":      {"", "INC-1"},
		"no correlation": {"operator asked", ""},
	} {
		if _, err := server.refundPrepaidRemainder(h.ctx, actor, buyerID, ref.reason, ref.correlation); !errors.Is(err, errAdminMutationInvalid) {
			t.Fatalf("%s: err = %v, want errAdminMutationInvalid", name, err)
		}
	}
	bal, _ := h.store.BuyerPrepaidBalanceMicros(h.ctx, buyerID)
	if bal != 25_000_000 {
		t.Fatalf("balance moved on a rejected refund: %d", bal)
	}
}

// TestPrepaidRefundLeavesOpenJobReservationsFunded pins defect (a): the refund
// read the gross balance, so refunding while jobs ran stripped the reservation
// those jobs were admitted against — the compute completed and settlement could
// never debit it.
func TestPrepaidRefundLeavesOpenJobReservationsFunded(t *testing.T) {
	h := newFundingHarness(t)
	buyerID := insertTestBuyer(t, h.pool, h.ctx)
	actor := insertTestAdminActor(t, h.pool, h.ctx)
	fundPrepaidViaTopup(t, h.ctx, h.store, buyerID, 5000) // $50

	// $30 of that is frozen behind a running prepay job.
	jobID := uuid.New()
	if _, err := h.pool.Exec(h.ctx, `
		INSERT INTO jobs (id,buyer_id,status,job_type,input_ref,task_count,prepaid_required)
		VALUES ($1,$2,'running','embed','x',1,true)`, jobID, buyerID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.pool.Exec(h.ctx, `
		INSERT INTO job_economic_plans
		  (job_id,plan_version,schedule_version,plan_json,initial_task_count,
		   buyer_charge_per_task_usd,supplier_payout_per_task_usd,
		   initial_buyer_charge_usd,reserved_buyer_charge_usd)
		VALUES ($1,1,'test','{"schedule":{"currency":"usd"}}',1,30.00,20.00,30.00,30.00)`,
		jobID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = h.pool.Exec(c, `DELETE FROM job_economic_plans WHERE job_id=$1`, jobID)
		_, _ = h.pool.Exec(c, `DELETE FROM jobs WHERE id=$1`, jobID)
	})

	server := &Server{store: h.store}
	result, err := server.refundPrepaidRemainder(h.ctx, actor, buyerID, "buyer closed account", "INC-reserve-"+uuid.NewString())
	if err != nil {
		t.Fatalf("refund: %v", err)
	}
	if got := result["refunded_micros"].(int64); got != 20_000_000 {
		t.Fatalf("refunded %d micro-USD, want 20000000 ($50 funded less the $30 still reserved)", got)
	}
	bal, _ := h.store.BuyerPrepaidBalanceMicros(h.ctx, buyerID)
	if bal != 30_000_000 {
		t.Fatalf("balance after refund = %d, want 30000000 still covering the running job", bal)
	}
	// The whole point: the running job's settlement can still be paid.
	if err := reservePrepaidCheck(h.ctx, h.store, buyerID, 30_000_000); err != nil {
		t.Fatalf("running job can no longer be settled from prepaid balance: %v", err)
	}
}

// reservePrepaidCheck asserts the buyer's balance can still absorb a settlement
// of the given size.
func reservePrepaidCheck(ctx context.Context, store *Store, buyerID uuid.UUID, micros int64) error {
	bal, err := store.BuyerPrepaidBalanceMicros(ctx, buyerID)
	if err != nil {
		return err
	}
	if bal < micros {
		return fmt.Errorf("balance %d micro-USD cannot cover a %d micro-USD settlement", bal, micros)
	}
	return nil
}

// TestPrepaidRefundCannotExceedFundedValue proves refunds are traced back to the
// payment intents that collected the cash, so a second refund cannot re-refund
// an intent the first one already emptied.
func TestPrepaidRefundCannotExceedFundedValue(t *testing.T) {
	h := newFundingHarness(t)
	buyerID := insertTestBuyer(t, h.pool, h.ctx)
	actor := insertTestAdminActor(t, h.pool, h.ctx)
	fundPrepaidViaTopup(t, h.ctx, h.store, buyerID, 2500)

	server := &Server{store: h.store}
	if _, err := server.refundPrepaidRemainder(h.ctx, actor, buyerID, "closed", "INC-a-"+uuid.NewString()); err != nil {
		t.Fatalf("first refund: %v", err)
	}
	// Balance is zero, so a second refund has nothing to return.
	_, err := server.refundPrepaidRemainder(h.ctx, actor, buyerID, "closed", "INC-b-"+uuid.NewString())
	if !errors.Is(err, errInsufficientPrepaid) {
		t.Fatalf("second refund err = %v, want errInsufficientPrepaid", err)
	}

	// Balance that no collected payment intent backs cannot be refunded at all:
	// seeded credit has no card behind it.
	if err := h.store.SeedPrepaidBalance(h.ctx, buyerID, 25_000_000, "seed-"+uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	_, err = server.refundPrepaidRemainder(h.ctx, actor, buyerID, "closed", "INC-c-"+uuid.NewString())
	if err == nil || !strings.Contains(err.Error(), "exceed funded value") {
		t.Fatalf("refund of uncollected balance err = %v, want a funded-value refusal", err)
	}
	if got := h.refunds.Load(); got != 1 {
		t.Fatalf("stripe refunds = %d, want 1", got)
	}
	bal, _ := h.store.BuyerPrepaidBalanceMicros(h.ctx, buyerID)
	if bal != 25_000_000 {
		t.Fatalf("balance after refused refund = %d, want the seeded 25000000 intact", bal)
	}
}

// TestPrepaidRefundIsDurableBeforeStripe pins defect (c): the provider call used
// to run before any durable row, so a crash returned the cash and left the same
// balance spendable. A failing provider must leave the money debited and the
// operation pending, never spendable twice.
func TestPrepaidRefundIsDurableBeforeStripe(t *testing.T) {
	h := newFundingHarness(t)
	buyerID := insertTestBuyer(t, h.pool, h.ctx)
	actor := insertTestAdminActor(t, h.pool, h.ctx)
	fundPrepaidViaTopup(t, h.ctx, h.store, buyerID, 2500)

	server := &Server{store: h.store}
	correlation := "INC-durable-" + uuid.NewString()
	h.refundErr.Store(true)
	if _, err := server.refundPrepaidRemainder(h.ctx, actor, buyerID, "closed", correlation); err == nil {
		t.Fatal("a failing Stripe refund reported success")
	}
	bal, _ := h.store.BuyerPrepaidBalanceMicros(h.ctx, buyerID)
	if bal != 0 {
		t.Fatalf("balance after a failed provider call = %d, want 0 (already debited, not spendable)", bal)
	}
	var pending int
	if err := h.pool.QueryRow(h.ctx, `
		SELECT count(*) FROM prepaid_refund_operations
		 WHERE buyer_id=$1 AND status='pending'`, buyerID).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if pending != 1 {
		t.Fatalf("pending refund operations = %d, want 1 recording the owed cash", pending)
	}

	// Replaying the same incident reference finishes the transfer instead of
	// debiting the buyer a second time.
	h.refundErr.Store(false)
	result, err := server.refundPrepaidRemainder(h.ctx, actor, buyerID, "closed", correlation)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if replayed, _ := result["replayed"].(bool); !replayed {
		t.Fatalf("replay was treated as a new refund: %v", result)
	}
	if got := result["refunded_cents"].(int64); got != 2500 {
		t.Fatalf("replay refunded %d cents, want 2500", got)
	}
	bal, _ = h.store.BuyerPrepaidBalanceMicros(h.ctx, buyerID)
	if bal != 0 {
		t.Fatalf("balance after replay = %d, want 0 (no second debit)", bal)
	}
	var succeeded int
	if err := h.pool.QueryRow(h.ctx, `
		SELECT count(*) FROM prepaid_refund_operations
		 WHERE buyer_id=$1 AND status='succeeded'`, buyerID).Scan(&succeeded); err != nil {
		t.Fatal(err)
	}
	if succeeded != 1 {
		t.Fatalf("succeeded refund operations = %d, want 1", succeeded)
	}
}
