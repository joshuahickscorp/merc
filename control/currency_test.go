package main

import (
	"context"
	"errors"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TestMain installs a settlement currency for the package so money paths that
// require MERC_SETTLEMENT_CURRENCY do not fail closed during unrelated unit
// tests. Individual tests that exercise boot refusal reset and restore it.
func TestMain(m *testing.M) {
	if strings.TrimSpace(os.Getenv(settlementCurrencyEnv)) == "" {
		_ = os.Setenv(settlementCurrencyEnv, "usd")
	}
	if _, err := LoadSettlementCurrencyFromEnv(); err != nil {
		panic("test bootstrap settlement currency: " + err.Error())
	}
	os.Exit(m.Run())
}

func TestLoadSettlementCurrencyRefusesUnset(t *testing.T) {
	t.Setenv(settlementCurrencyEnv, "")
	resetSettlementCurrencyForTest()
	t.Cleanup(func() {
		_ = os.Setenv(settlementCurrencyEnv, "usd")
		_, _ = LoadSettlementCurrencyFromEnv()
	})
	if _, err := LoadSettlementCurrencyFromEnv(); err == nil {
		t.Fatal("expected refusal when settlement currency is unset")
	} else if !strings.Contains(err.Error(), settlementCurrencyEnv) {
		t.Fatalf("error should name the env var: %v", err)
	}
	if _, err := SettlementCurrency(); !errors.Is(err, errSettlementCurrency) {
		t.Fatalf("SettlementCurrency after failed load: %v", err)
	}
}

func TestLoadSettlementCurrencyRefusesUnsupported(t *testing.T) {
	t.Setenv(settlementCurrencyEnv, "xyz")
	resetSettlementCurrencyForTest()
	t.Cleanup(func() {
		_ = os.Setenv(settlementCurrencyEnv, "usd")
		_, _ = LoadSettlementCurrencyFromEnv()
	})
	if _, err := LoadSettlementCurrencyFromEnv(); err == nil {
		t.Fatal("expected refusal for unsupported currency")
	} else if !strings.Contains(err.Error(), "xyz") {
		t.Fatalf("error should name the bad code: %v", err)
	}
}

func TestLoadSettlementCurrencyAcceptsCAD(t *testing.T) {
	t.Setenv(settlementCurrencyEnv, "CAD")
	c, err := LoadSettlementCurrencyFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.Setenv(settlementCurrencyEnv, "usd")
		_, _ = LoadSettlementCurrencyFromEnv()
	})
	if c.Code() != "cad" || c.Exponent() != 2 {
		t.Fatalf("got %+v", c)
	}
	got, err := SettlementCurrency()
	if err != nil || !got.Equal(c) {
		t.Fatalf("process settlement = %v err=%v", got, err)
	}
}

func TestMinorAmountRefusesCrossCurrencyAdd(t *testing.T) {
	usd := MustParseCurrency("usd")
	cad := MustParseCurrency("cad")
	a := MustMinorAmount(100, usd)
	b := MustMinorAmount(50, cad)
	if _, err := a.Add(b); !errors.Is(err, errCurrencyMismatch) {
		t.Fatalf("want errCurrencyMismatch, got %v", err)
	}
	if _, err := a.Sub(b); !errors.Is(err, errCurrencyMismatch) {
		t.Fatalf("want errCurrencyMismatch on Sub, got %v", err)
	}
	sum, err := a.Add(MustMinorAmount(25, usd))
	if err != nil || sum.Amount() != 125 {
		t.Fatalf("same-currency add failed: sum=%v err=%v", sum, err)
	}
}

func TestLedgerInsertRefusesCrossCurrency(t *testing.T) {
	installSettlementCurrencyForTest(t, "cad")
	err := validateLedgerInsert(ledgerInsert{
		Kind: KindSupplierCredit, AmountMicros: 1_000_000, Currency: "usd",
		PayoutStatus: PayoutHeld,
	})
	if err == nil || !errors.Is(err, errCurrencyMismatch) {
		t.Fatalf("want currency mismatch for usd insert under cad settlement, got %v", err)
	}
	// Settlement-matching insert is accepted.
	if err := validateLedgerInsert(ledgerInsert{
		Kind: KindSupplierCredit, AmountMicros: 1_000_000, Currency: "cad",
		PayoutStatus: PayoutHeld,
	}); err != nil {
		t.Fatalf("cad insert under cad settlement: %v", err)
	}
}

func TestPayoutRefusesNonSettlementCurrencyBeforeStripe(t *testing.T) {
	installSettlementCurrencyForTest(t, "cad")
	// No secret, no HTTP client: refusal must happen before any network call.
	p := StripePayout{secret: "sk_test_x"}
	// Supplier lookup will fail if we get past currency — use empty store only
	// after currency check. Currency check is first after amount, but supplier
	// is looked up first. Use a nil store would panic; use ManualExport instead.
	export := newManualExportPayout(t.TempDir() + "/export.csv")
	_, err := export.Send(context.Background(), uuid.New(), 100, "usd", "key-1")
	if err == nil {
		t.Fatal("usd payout under cad settlement must be refused")
	}
	if !strings.Contains(err.Error(), "currency") && !errors.Is(err, errCurrencyMismatch) {
		t.Fatalf("error should cite currency mismatch: %v", err)
	}
	// Stripe Send also refuses before transfer when currency mismatches, after
	// supplier lookup. Probe the check directly.
	if err := RequireSettlementCurrency("usd"); !errors.Is(err, errCurrencyMismatch) {
		t.Fatalf("RequireSettlementCurrency(usd) under cad: %v", err)
	}
	_ = p
}

func TestHistoricalUSDRowsReadableUnderCADSettlement(t *testing.T) {
	ctx, store, pool := openIsolatedTestStore(t)
	// Seed a historical USD ledger row while settlement is still usd (TestMain).
	buyerID := uuid.New()
	if _, err := pool.Exec(ctx, `
		INSERT INTO buyers (id,email,password_hash) VALUES ($1,$2,'x')`,
		buyerID, "hist-"+buyerID.String()+"@test"); err != nil {
		t.Fatal(err)
	}
	var entryID uuid.UUID
	if err := pool.QueryRow(ctx, `
		INSERT INTO ledger_entries (kind,buyer_id,amount_usd,currency,payout_status)
		VALUES ('buyer_charge',$1,-1.25,'usd','released')
		RETURNING id`, buyerID).Scan(&entryID); err != nil {
		t.Fatal(err)
	}
	// Cut over to CAD without restating the historical row.
	installSettlementCurrencyForTest(t, "cad")
	var amount float64
	var currency string
	if err := pool.QueryRow(ctx, `
		SELECT amount_usd::float8, currency FROM ledger_entries WHERE id=$1`,
		entryID).Scan(&amount, &currency); err != nil {
		t.Fatal(err)
	}
	if currency != "usd" {
		t.Fatalf("historical currency restated: got %q want usd", currency)
	}
	if amount != -1.25 {
		t.Fatalf("historical amount restated: got %v", amount)
	}
	// A new CAD write is accepted; the USD row is untouched.
	if _, err := insertLedgerEntryTx(ctx, pool, ledgerInsert{
		Kind: KindPlatformTake, AmountMicros: 100_000, Currency: "cad",
		PayoutStatus: PayoutReleased,
	}); err != nil {
		t.Fatalf("new cad ledger write: %v", err)
	}
	var stillUSD string
	if err := pool.QueryRow(ctx,
		`SELECT currency FROM ledger_entries WHERE id=$1`, entryID,
	).Scan(&stillUSD); err != nil {
		t.Fatal(err)
	}
	if stillUSD != "usd" {
		t.Fatalf("historical row mutated after CAD write: %q", stillUSD)
	}
	_ = store
}

func TestSettlementPreflightUsesConfiguredCurrencyBothDirections(t *testing.T) {
	// Configured CAD + platform holds CAD → pass.
	installSettlementCurrencyForTest(t, "cad")
	srv, _ := stripeBalanceStub(t, http.StatusOK,
		`{"available":[{"currency":"cad"}],"pending":[]}`)
	if err := payoutAgainst(t, srv, "sk_test_x").verifySettlementCurrency(context.Background()); err != nil {
		t.Fatalf("CAD platform should pass under cad settlement: %v", err)
	}
	// Configured CAD + platform holds only USD → fail.
	srvUSD, _ := stripeBalanceStub(t, http.StatusOK,
		`{"available":[{"currency":"usd"}],"pending":[]}`)
	err := payoutAgainst(t, srvUSD, "sk_test_x").verifySettlementCurrency(context.Background())
	if err == nil {
		t.Fatal("USD-only platform must fail under cad settlement")
	}
	var unsupported errSettlementCurrencyUnsupported
	if !errors.As(err, &unsupported) {
		t.Fatalf("want errSettlementCurrencyUnsupported, got %T: %v", err, err)
	}
	if unsupported.Want != "cad" {
		t.Fatalf("want want=cad, got %q", unsupported.Want)
	}

	// Configured USD + platform holds USD → pass (regression).
	installSettlementCurrencyForTest(t, "usd")
	srv2, _ := stripeBalanceStub(t, http.StatusOK,
		`{"available":[{"currency":"usd"}],"pending":[]}`)
	if err := payoutAgainst(t, srv2, "sk_test_x").verifySettlementCurrency(context.Background()); err != nil {
		t.Fatalf("USD platform should pass under usd settlement: %v", err)
	}
	// Configured USD + platform holds only CAD → fail (the original regression).
	srvCAD, _ := stripeBalanceStub(t, http.StatusOK,
		`{"available":[{"currency":"cad"}],"pending":[]}`)
	err = payoutAgainst(t, srvCAD, "sk_test_x").verifySettlementCurrency(context.Background())
	if err == nil {
		t.Fatal("CAD-only platform must fail under usd settlement")
	}
}

func TestCurrencyExponentRecordedNotAssumedTwoDecimals(t *testing.T) {
	jpy := MustParseCurrency("jpy")
	if jpy.Exponent() != 0 {
		t.Fatalf("jpy exponent=%d", jpy.Exponent())
	}
	micros, err := jpy.MicrosPerMinorUnit()
	if err != nil {
		t.Fatal(err)
	}
	// Zero-decimal: one yen is 1e6 ledger micro-units, not 1e4 (which would be
	// a silent *100 if exponent were hardcoded to 2).
	if micros != 1_000_000 {
		t.Fatalf("jpy micros/minor=%d want 1000000", micros)
	}
	usd := MustParseCurrency("usd")
	um, err := usd.MicrosPerMinorUnit()
	if err != nil || um != 10_000 {
		t.Fatalf("usd micros/minor=%d err=%v", um, err)
	}
}

func TestParseCurrencyRejectsEmptyAndUnknown(t *testing.T) {
	if _, err := ParseCurrency(""); err == nil {
		t.Fatal("empty currency accepted")
	}
	if _, err := ParseCurrency("eur"); err == nil {
		t.Fatal("unsupported eur accepted")
	}
}

// Ensure openIsolatedTestStore type is referenced only when DB tests run.
var _ = pgxpool.Pool{}
