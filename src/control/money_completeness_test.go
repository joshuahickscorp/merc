package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TestNoRawLedgerInsertsOutsideWriter is the CI-checkable 4.3 guard: every
// production INSERT INTO ledger_entries must live in ledger_write.go.
func TestNoRawLedgerInsertsOutsideWriter(t *testing.T) {
	entries, err := os.ReadDir(".")
	must(t, err)
	const needle = "INSERT INTO ledger_entries"
	var offenders []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") {
			continue
		}
		if name == "ledger_write.go" || strings.HasSuffix(name, "_test.go") {
			continue
		}
		body, err := os.ReadFile(name)
		must(t, err)
		if strings.Contains(string(body), needle) {
			offenders = append(offenders, name)
		}
	}
	if len(offenders) > 0 {
		t.Fatalf("raw %q outside ledger_write.go: %v", needle, offenders)
	}
}

func TestRoundUSDUnifiesHistoricalHelpers(t *testing.T) {
	for _, v := range []float64{0, 1.2345674, 1.2345675, -0.0000006, 50000.123456} {
		if got, want := roundUSD(v), roundEconomicUSD(v); got != want {
			t.Fatalf("roundUSD(%v)=%v roundEconomicUSD=%v", v, got, want)
		}
	}
}

func TestMoneyUSDDomainBounds(t *testing.T) {
	if !moneyUSDInDomain(50000.123456) {
		t.Fatal("50k must fit NUMERIC(12,6)")
	}
	if moneyUSDInDomain(1_000_000) {
		t.Fatal("1e6 must exceed NUMERIC(12,6)")
	}
	if moneyUSDInDomain(maxMoneyUSD + 0.000001) {
		t.Fatal("just over max must be rejected")
	}
	if moneyUSDInDomain(maxMoneyUSD) != true {
		t.Fatal("exact max must be accepted")
	}
}

func TestMoneyDomainRejectsOversizeValues(t *testing.T) {
	if moneyUSDInDomain(2_000_000) {
		t.Fatal("2e6 must exceed domain")
	}
	if !moneyUSDInDomain(50000.123456) {
		t.Fatal("50_000.123456 must fit domain (proves old NUMERIC(10,6) was insufficient)")
	}
	if usdToMicros(50000.123456) != 50_000_123_456 {
		t.Fatalf("micros = %d", usdToMicros(50000.123456))
	}
}

func configureStripeHTTPShapeAuthority(t *testing.T, secret string) {
	t.Helper()
	t.Setenv("MERC_ENV", "development")
	t.Setenv(paymentModeEnv, string(PaymentModeTest))
	t.Setenv(stripeSecretKeyFileEnv, "")
	t.Setenv("STRIPE_SECRET_KEY", secret)
}

func TestStripeTransferReversalHTTPIdempotentShape(t *testing.T) {
	configureStripeHTTPShapeAuthority(t, "sk_test_reversal_shape")
	var seenKeys []string
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		seenKeys = append(seenKeys, r.Header.Get("Idempotency-Key"))
		if got := r.Header.Get("Stripe-Version"); got != stripeAPIVersion {
			t.Errorf("Stripe-Version = %q, want %q", got, stripeAPIVersion)
		}
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/reversals") {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			http.Error(w, "bad path", 500)
			return
		}
		_ = r.ParseForm()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"object": "transfer_reversal", "id": "trr_sim_1", "amount": 125, "currency": "usd",
		})
	}))
	defer srv.Close()

	// Point StripePayout at the test server by overriding the client transport
	// via a custom http.Client that rewrites the host. Easier: inject base via
	// temporary monkey — we call the method with a rewritten client that posts
	// to the test server by replacing DefaultTransport.
	p := StripePayout{
		secret: "sk_test_reversal_shape",
		http: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			req.URL.Scheme = "http"
			req.URL.Host = strings.TrimPrefix(srv.URL, "http://")
			return http.DefaultTransport.RoundTrip(req)
		})},
	}
	key := uuid.NewString()
	first, err := p.ReverseTransfer(context.Background(), "tr_sim", 125, "usd", key)
	mustf(t, err, "first reverse: %v")
	second, err := p.ReverseTransfer(context.Background(), "tr_sim", 125, "usd", key)
	mustf(t, err, "idempotent reverse: %v")
	if first.Ref != "trr_sim_1" || second.Ref != first.Ref {
		t.Fatalf("results = %+v %+v", first, second)
	}
	if first.Instrument != "transfer_reversal" {
		t.Fatalf("instrument = %q", first.Instrument)
	}
	wantKey := stripeReversalIdempotencyKey(key)
	for _, k := range seenKeys {
		if k != wantKey {
			t.Fatalf("idempotency key %q want %q", k, wantKey)
		}
	}
	if calls != 2 {
		t.Fatalf("calls=%d want 2 (idempotent retry still hits transport; Stripe would dedupe)", calls)
	}
}

func TestStripeChargeRefundHTTPShape(t *testing.T) {
	configureStripeHTTPShapeAuthority(t, "sk_test_refund_shape")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Stripe-Version"); got != stripeAPIVersion {
			t.Errorf("Stripe-Version = %q, want %q", got, stripeAPIVersion)
		}
		if r.URL.Path != "/v1/refunds" {
			t.Errorf("path = %s", r.URL.Path)
		}
		_ = r.ParseForm()
		if r.Form.Get("payment_intent") != "pi_sim" || r.Form.Get("amount") != "50" {
			t.Errorf("form = %v", r.Form)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"object": "refund", "id": "re_sim_1", "amount": 50, "currency": "usd",
		})
	}))
	defer srv.Close()
	p := StripePayout{
		secret: "sk_test_refund_shape",
		http: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			req.URL.Scheme = "http"
			req.URL.Host = strings.TrimPrefix(srv.URL, "http://")
			return http.DefaultTransport.RoundTrip(req)
		})},
	}
	got, err := p.RefundCharge(context.Background(), "pi_sim", 50, "usd", "entry-1")
	must(t, err)
	if got.Ref != "re_sim_1" || got.Instrument != "charge_refund" {
		t.Fatalf("got = %+v", got)
	}
}

func TestStripeMoneyObjectRequiresExpectedObjectKind(t *testing.T) {
	for _, tc := range []struct {
		name   string
		body   string
		prefix string
		wantID string
	}{
		{name: "transfer", body: `{"object":"transfer","id":"tr_sim_1","amount":50,"currency":"USD"}`, prefix: "tr_", wantID: "tr_sim_1"},
		{name: "reversal", body: `{"object":"transfer_reversal","id":"trr_sim_1","amount":50,"currency":"usd"}`, prefix: "trr_", wantID: "trr_sim_1"},
		{name: "refund", body: `{"object":"refund","id":"re_sim_1","amount":50,"currency":"usd"}`, prefix: "re_", wantID: "re_sim_1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseStripeMoneyObject([]byte(tc.body), tc.name, tc.prefix, 50, "usd")
			mustf(t, err, "parse %s: %v", tc.name, err)
			if got.ID != tc.wantID || got.Currency != "usd" {
				t.Fatalf("object = %+v", got)
			}
		})
	}
	for _, tc := range []struct {
		name   string
		body   string
		prefix string
	}{
		{name: "transfer rejects refund", body: `{"id":"re_wrong","amount":50,"currency":"usd"}`, prefix: "tr_"},
		{name: "reversal rejects transfer", body: `{"id":"tr_wrong","amount":50,"currency":"usd"}`, prefix: "trr_"},
		{name: "refund rejects payment intent", body: `{"id":"pi_wrong","amount":50,"currency":"usd"}`, prefix: "re_"},
		{name: "missing object kind", body: `{"id":"tr_missing_object","amount":50,"currency":"usd"}`, prefix: "tr_"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parseStripeMoneyObject([]byte(tc.body), tc.name, tc.prefix, 50, "usd"); err == nil {
				t.Fatal("wrong Stripe object kind was accepted")
			}
		})
	}
}

func TestStripePrepaidRefundResponseBindsDurableSlice(t *testing.T) {
	base := func() map[string]any {
		return map[string]any{
			"object":         "refund",
			"id":             "re_sim_1",
			"amount":         float64(50),
			"currency":       "usd",
			"payment_intent": "pi_sim_1",
			"status":         "succeeded",
		}
	}
	got, err := parseStripePrepaidRefundResponse(base(), "pi_sim_1", 50, "USD")
	must(t, err)
	if got != "re_sim_1" {
		t.Fatalf("refund id = %q", got)
	}

	for _, tc := range []struct {
		name   string
		mutate func(map[string]any)
	}{
		{name: "wrong object", mutate: func(out map[string]any) { out["object"] = "charge" }},
		{name: "wrong amount", mutate: func(out map[string]any) { out["amount"] = float64(49) }},
		{name: "wrong currency", mutate: func(out map[string]any) { out["currency"] = "cad" }},
		{name: "wrong payment intent", mutate: func(out map[string]any) { out["payment_intent"] = "pi_other" }},
		{name: "wrong expanded payment intent kind", mutate: func(out map[string]any) {
			out["payment_intent"] = map[string]any{"object": "charge", "id": "pi_sim_1"}
		}},
		{name: "missing payment intent", mutate: func(out map[string]any) { delete(out, "payment_intent") }},
		{name: "pending status", mutate: func(out map[string]any) { out["status"] = "pending" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := base()
			tc.mutate(out)
			if _, err := parseStripePrepaidRefundResponse(out, "pi_sim_1", 50, "usd"); err == nil {
				t.Fatal("mismatched refund response was accepted")
			}
		})
	}
}

func TestStripePayoutRecoveryRejectsWrongProviderInputBeforeHTTP(t *testing.T) {
	configureStripeHTTPShapeAuthority(t, "sk_test_recovery_input_shape")
	calls := 0
	p := StripePayout{
		secret: "sk_test_recovery_input_shape",
		http: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			calls++
			return nil, errors.New("provider must not be reached")
		})},
	}
	if _, err := p.ReverseTransfer(context.Background(), "pi_not_a_transfer", 50, "usd", "reverse-1"); !errors.Is(err, errPayoutDefinitelyNotSent) {
		t.Fatalf("wrong transfer input error = %v", err)
	}
	if _, err := p.RefundCharge(context.Background(), "ch_not_a_payment_intent", 50, "usd", "reverse-2"); !errors.Is(err, errPayoutDefinitelyNotSent) {
		t.Fatalf("wrong payment intent input error = %v", err)
	}
	if calls != 0 {
		t.Fatalf("provider calls = %d, want 0", calls)
	}
}

func TestStripePayoutRequiresDurableKeyBeforeProvider(t *testing.T) {
	configureStripeHTTPShapeAuthority(t, "sk_test_payout_key_shape")
	calls := 0
	p := StripePayout{
		secret: "sk_test_payout_key_shape",
		http: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			calls++
			return nil, errors.New("provider must not be reached")
		})},
	}
	for _, key := range []string{"", "   "} {
		if _, err := p.Send(context.Background(), uuid.New(), 50, "usd", key); !errors.Is(err, errPayoutDefinitelyNotSent) {
			t.Fatalf("empty payout key %q error=%v, want definite refusal", key, err)
		}
	}
	if calls != 0 {
		t.Fatalf("provider calls = %d, want 0", calls)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// TestSimulatorPayoutReversalIdempotent exercises the existing deterministic
// Stripe simulator reverse path. See the report for what this does *not* prove.
func TestSimulatorPayoutReversalIdempotent(t *testing.T) {
	sim := newDeterministicStripeSimulator(42)
	intent, err := sim.createPaymentIntent("rev-key", 1000, "attempt-rev", true)
	must(t, err)
	must(t, sim.authorize(intent.ID))
	must(t, sim.capture(intent.ID))
	must(t, sim.transfer(intent.ID, "xfer-1", "attempt-rev", 400))
	must(t, sim.payout(intent.ID, "release-1", "released"))
	must(t, sim.payout(intent.ID, "reverse-1", "reversed"))
	snap, _, err := sim.snapshot(intent.ID)
	must(t, err)
	if snap.PayoutState != "reversed" {
		t.Fatalf("payout state = %q", snap.PayoutState)
	}
	// Idempotent reverse effect key does not double-book the ledger.
	mustf(t, sim.payout(intent.ID, "reverse-1", "reversed"), "idempotent reverse: %v")
	// A second distinct reverse from already-reversed is rejected.
	if err := sim.payout(intent.ID, "reverse-2", "reversed"); err == nil {
		t.Fatal("second reverse effect from reversed state should fail")
	}
}

func TestReversalPayoutPauseAndResumeIntegration(t *testing.T) {
	// Own database: the payout pause is platform-wide, so this assertion is only
	// meaningful when nothing else can hold the platform paused.
	ctx, store, pool := openIsolatedTestStore(t)
	t.Setenv("MERC_CANARY_MODE", "false")
	// Disabling canary now requires a recorded decision reference, because the
	// same switch opens self-serve signup.  This test wants the non-canary
	// payout path, so it records one rather than leaving the pair ambiguous.
	t.Setenv("MERC_CANARY_DISABLE_DECISION_REF", "TEST-reversal-payout-pause")

	// The payout pause is a PLATFORM-WIDE behaviour -- it engages while any
	// ledger row is reversal_required anywhere -- so this test has to own that
	// state rather than assert around it.  Sibling reversal tests leave rows
	// behind, and "outstanding == 0" is only true on a database nobody else has
	// touched.

	// Due supplier credit that would otherwise be claimed.
	due := seedHeldDuePayout(t, ctx, pool, 1.25)
	// Unrelated row stuck in reversal_required — global pause must engage.
	blocking := uuid.New()
	if _, err := pool.Exec(ctx, `
		INSERT INTO ledger_entries (id, kind, amount_usd, payout_status)
		VALUES ($1, 'supplier_credit', 0.01, 'reversal_required')`, blocking); err != nil {
		t.Fatalf("seed reversal_required: %v", err)
	}

	// CountReversalRequired is platform-wide and supplier_payout_operations is
	// deliberately append-only, so sibling tests' rows cannot be cleaned up and
	// "outstanding == 0" is unreachable on a shared database.  Baseline it and
	// assert the DELTA this test causes, which is the actual property: adding a
	// reversal_required row pauses the sweep, and clearing it releases the pause.
	if n, err := store.CountReversalRequired(ctx); err != nil || n != 1 {
		t.Fatalf("expected exactly this test's one outstanding reversal, got %d err=%v", n, err)
	}
	beforePause := metrics.payoutsPausedReversalRequired.Load()
	wk := NewWorkers(store, nil, stubPayout{})
	mustf(t, wk.releasePayouts(ctx), "paused sweep: %v")
	if metrics.payoutsPausedReversalRequired.Load() != beforePause+1 {
		t.Fatal("expected pause metric bump")
	}
	var dueStatus string
	if err := pool.QueryRow(ctx, `SELECT payout_status FROM ledger_entries WHERE id=$1`, due).
		Scan(&dueStatus); err != nil {
		t.Fatal(err)
	}
	if dueStatus != PayoutHeld {
		t.Fatalf("due credit advanced under pause: status=%q", dueStatus)
	}
	// Deliberately no direct ClaimPayout probe here.  The pause is enforced at
	// the worker sweep, so a direct claim is expected to succeed -- but it also
	// advances the row out of 'held', which is exactly the state the resume
	// assertions below depend on.  A probe that asserts nothing and destroys the
	// fixture is worse than no probe.

	// Clear the blocking row; sweep must resume claiming.
	if _, err := pool.Exec(ctx, `DELETE FROM ledger_entries WHERE id=$1`, blocking); err != nil {
		t.Fatal(err)
	}
	n, err := store.CountReversalRequired(ctx)
	must(t, err)
	if n != 0 {
		t.Fatalf("outstanding after clear = %d, want 0 on an isolated database", n)
	}
	entries, err := store.DuePayouts(ctx, 100)
	must(t, err)
	if !dueContains(entries, due) {
		t.Fatal("due credit not visible after clearing reversal_required")
	}
	// Re-run sweep: must not bump pause metric, and must advance the due row
	// out of held (no funding → awaiting_funding is still a successful claim CAS).
	beforeResume := metrics.payoutsPausedReversalRequired.Load()
	mustf(t, wk.releasePayouts(ctx), "resumed sweep: %v")
	if metrics.payoutsPausedReversalRequired.Load() != beforeResume {
		t.Fatal("pause metric bumped after reversal_required cleared")
	}
	if err := pool.QueryRow(ctx, `SELECT payout_status FROM ledger_entries WHERE id=$1`, due).
		Scan(&dueStatus); err != nil {
		t.Fatal(err)
	}
	if dueStatus == PayoutHeld {
		t.Fatal("resumed sweep left due credit unclaimed (still held)")
	}
}

func seedHeldDuePayout(t *testing.T, ctx context.Context, pool *pgxpool.Pool, amount float64) uuid.UUID {
	t.Helper()
	supplierID, jobID, taskID, entryID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	for _, statement := range []struct {
		q    string
		args []any
	}{
		{`INSERT INTO suppliers (id,email,reputation,status) VALUES ($1,$2,0.5,'active')`,
			[]any{supplierID, supplierID.String() + "@money.invalid"}},
		{`INSERT INTO jobs (id,buyer_id,status,job_type,input_ref)
		  VALUES ($1,$2,'complete','embed','money/input')`, []any{jobID, uuid.New()}},
		{`INSERT INTO tasks (id,job_id,status,verification_outcome,completed_at)
		  VALUES ($1,$2,'complete','pass',now())`, []any{taskID, jobID}},
		{`INSERT INTO ledger_entries
		    (id,kind,supplier_id,task_id,amount_usd,payout_status,release_at)
		  VALUES ($1,'supplier_credit',$2,$3,$4,'held',now()-interval '1 minute')`,
			[]any{entryID, supplierID, taskID, amount}},
	} {
		if _, err := pool.Exec(ctx, statement.q, statement.args...); err != nil {
			t.Fatalf("seed held due payout: %v", err)
		}
	}
	return entryID
}

func TestLedgerAmountDomainWideIntegration(t *testing.T) {
	ctx, _, pool := openAdminMutationTestStore(t)
	// $50,000.123456 must succeed under NUMERIC(12,6) and would fail under the
	// old NUMERIC(10,6) domain (max $9,999.999999).
	id := uuid.New()
	const amount = 50000.123456
	if _, err := pool.Exec(ctx, `
		INSERT INTO ledger_entries (id, kind, amount_usd, payout_status)
		VALUES ($1, 'supplier_credit', $2, 'held')`, id, amount); err != nil {
		t.Fatalf("insert $%v supplier credit: %v", amount, err)
	}
	var got float64
	var typ string
	if err := pool.QueryRow(ctx, `
		SELECT amount_usd::float8,
		       (SELECT format_type(a.atttypid, a.atttypmod)
		          FROM pg_attribute a
		          JOIN pg_class c ON c.oid = a.attrelid
		         WHERE c.relname = 'ledger_entries' AND a.attname = 'amount_usd'
		           AND a.attnum > 0 AND NOT a.attisdropped)
		  FROM ledger_entries WHERE id = $1`, id).Scan(&got, &typ); err != nil {
		t.Fatal(err)
	}
	if got != amount {
		t.Fatalf("amount = %v want %v", got, amount)
	}
	if !strings.Contains(typ, "numeric(12,6)") {
		t.Fatalf("ledger_entries.amount_usd type = %q, want numeric(12,6); un-migrated NUMERIC(10,6) would fail this test", typ)
	}
}

func TestReversalCASTerminalIntegration(t *testing.T) {
	t.Parallel()
	// Own database: processReversals claims platform-wide, so a sibling test's
	// pending row would be swept by this test and vice versa.
	ctx, store, pool := openIsolatedTestStore(t)
	supplierID, entryID := uuid.New(), uuid.New()
	if _, err := pool.Exec(ctx, `
		INSERT INTO suppliers (id,email,reputation,status) VALUES ($1,$2,0.5,'active')`,
		supplierID, supplierID.String()+"@rev.invalid"); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO ledger_entries (id,kind,supplier_id,amount_usd,payout_status,payout_ref)
		VALUES ($1,'supplier_credit',$2,1.00,'reversal_required','tr_test_1')`,
		entryID, supplierID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO supplier_payout_operations
		  (ledger_entry_id,supplier_id,requested_cents,sent_cents,currency,status,
		   cash_moved,transfer_ref)
		VALUES ($1,$2,100,100,'usd','reversal_required',true,$3)`,
		entryID, supplierID, "tr_test_"+uuid.NewString()); err != nil {
		t.Fatal(err)
	}

	// Make the row immediately claimable (updated_at in the past for stale reclaim path).
	if _, err := pool.Exec(ctx, `
		UPDATE supplier_payout_operations SET updated_at = now() - interval '1 hour'
		 WHERE ledger_entry_id = $1`, entryID); err != nil {
		t.Fatal(err)
	}

	claimed, err := store.ClaimReversals(ctx, time.Minute, 10)
	must(t, err)
	// ClaimReversals is platform-wide, so assert on THIS entry rather than on the
	// size of the whole result: a sibling test's pending row is not this test's
	// failure, and asserting an absolute count only holds on a database nobody
	// else has touched.
	mine := 0
	for _, c := range claimed {
		if c.ID == entryID {
			mine++
		}
	}
	if mine != 1 {
		t.Fatalf("claimed this entry %d times, want 1: %+v", mine, claimed)
	}
	// Second claim while reversing must not re-lease this entry (lease unexpired).
	again, err := store.ClaimReversals(ctx, time.Hour, 10)
	must(t, err)
	for _, c := range again {
		if c.ID == entryID {
			t.Fatalf("entry re-leased while still reversing: %+v", c)
		}
	}

	if _, err := store.FinalizeReversal(ctx, entryID, ReversalResult{
		Ref: "fake", Cents: 100, Currency: "usd", Instrument: "transfer_reversal",
	}); err == nil {
		t.Fatal("FinalizeReversal accepted an arbitrary provider reference")
	}

	// One ref, generated per run so it is unique against the UNIQUE index, and
	// then REPLAYED -- idempotency means the same reference arriving twice, which
	// is what a provider retry actually looks like.  A second, different ref is
	// not a replay and the store is right to refuse it.
	reversalRef := "trr_" + uuid.NewString()
	state, err := store.FinalizeReversal(ctx, entryID, ReversalResult{
		Ref: reversalRef, Cents: 100, Currency: "usd", Instrument: "transfer_reversal",
	})
	if err != nil || state != PayoutReversed {
		t.Fatalf("finalize = %q %v", state, err)
	}
	state, err = store.FinalizeReversal(ctx, entryID, ReversalResult{
		Ref: reversalRef, Cents: 100, Currency: "usd", Instrument: "transfer_reversal",
	})
	if err != nil || state != PayoutReversed {
		t.Fatalf("idempotent finalize = %q %v", state, err)
	}

	// Worker reverse with fake reverser after re-seeding a second row.
	entry2 := uuid.New()
	if _, err := pool.Exec(ctx, `
		INSERT INTO ledger_entries (id,kind,supplier_id,amount_usd,payout_status,payout_ref)
		VALUES ($1,'supplier_credit',$2,2.00,'reversal_required','tr_test_2')`,
		entry2, supplierID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO supplier_payout_operations
		  (ledger_entry_id,supplier_id,requested_cents,sent_cents,currency,status,
		   cash_moved,transfer_ref,updated_at)
		VALUES ($1,$2,200,200,'usd','reversal_required',true,$3, now()-interval '1 hour')`,
		entry2, supplierID, "tr_test_"+uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	wk := NewWorkers(store, nil, fakeReverserPayout{})
	must(t, wk.processReversals(ctx))
	var status string
	if err := pool.QueryRow(ctx, `SELECT payout_status FROM ledger_entries WHERE id=$1`, entry2).
		Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != PayoutReversed {
		t.Fatalf("worker reverse status = %q", status)
	}
}

// fakeReverserPayout implements Send (unused) and reverse instruments.
type fakeReverserPayout struct{}

func (fakeReverserPayout) Send(context.Context, uuid.UUID, int64, string, string) (PayoutResult, error) {
	return PayoutResult{}, payoutDefinitelyNotSent(errPayoutUnconfigured)
}

func (fakeReverserPayout) ReverseTransfer(_ context.Context, transferRef string, cents int64, currency, reverseKey string) (ReversalResult, error) {
	return ReversalResult{
		Ref: "trr_" + reverseKey, Cents: cents, Currency: currency, Instrument: "transfer_reversal",
	}, nil
}

func (fakeReverserPayout) RefundCharge(_ context.Context, pi string, cents int64, currency, reverseKey string) (ReversalResult, error) {
	return ReversalResult{
		Ref: "re_" + reverseKey, Cents: cents, Currency: currency, Instrument: "charge_refund",
	}, nil
}
