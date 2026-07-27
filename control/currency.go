package main

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
)

// Settlement currency is an explicit, process-wide configuration.
//
// The ledger, charges, payouts and receipts all settle in ONE currency per
// deployment. That currency is not a code literal: it is loaded once at boot
// from MERC_SETTLEMENT_CURRENCY, validated against the supported registry, and
// refused when unset or unknown. A silent default to "usd" is how a CAD Stripe
// platform became invisible until every supplier payout failed.
//
// Historical rows keep the currency they settled in. New writes must match the
// configured settlement currency. Cross-currency arithmetic is refused at the
// type boundary (MinorAmount), not merely discouraged by convention.

const (
	settlementCurrencyEnv = "MERC_SETTLEMENT_CURRENCY"
	// ledgerDecimalPlaces is the fixed scale of amount_usd NUMERIC(12,6).
	// Minor-unit conversion uses (ledgerDecimalPlaces - Currency.Exponent()).
	ledgerDecimalPlaces = 6
)

// Currency is a validated ISO 4217 code with its minor-unit exponent.
// The zero value is invalid and fails every operation closed.
type Currency struct {
	code     string
	exponent int // decimal places in the ISO minor unit (USD/CAD=2, JPY=0, …)
}

// supportedCurrencies is the closed set the control plane may settle or store.
// Adding a currency here does not enable it for a deployment — operators still
// set MERC_SETTLEMENT_CURRENCY, and Stripe must hold a matching balance bucket.
var supportedCurrencies = map[string]Currency{
	"usd": {code: "usd", exponent: 2},
	"cad": {code: "cad", exponent: 2},
	// Registered so a future zero-decimal settlement cannot silently multiply
	// by 100; not enabled as a recommended production settlement currency.
	"jpy": {code: "jpy", exponent: 0},
}

// SupportedCurrencyCodes returns the closed set for schema CHECKs and docs.
func SupportedCurrencyCodes() []string {
	out := make([]string, 0, len(supportedCurrencies))
	for code := range supportedCurrencies {
		out = append(out, code)
	}
	// Stable order for constraints and error messages.
	// Prefer alphabetical so SQL CHECK lists stay deterministic.
	for i := 0; i < len(out); i++ {
		for j := i + 1; j < len(out); j++ {
			if out[j] < out[i] {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

// ParseCurrency validates and returns a supported Currency. Unknown or empty
// codes fail closed.
func ParseCurrency(raw string) (Currency, error) {
	code := strings.ToLower(strings.TrimSpace(raw))
	if code == "" {
		return Currency{}, errors.New("currency is required")
	}
	c, ok := supportedCurrencies[code]
	if !ok {
		return Currency{}, fmt.Errorf("unsupported currency %q", raw)
	}
	return c, nil
}

// MustParseCurrency is for package-level constants of known-good codes only.
func MustParseCurrency(raw string) Currency {
	c, err := ParseCurrency(raw)
	if err != nil {
		panic(err)
	}
	return c
}

func (c Currency) Valid() bool {
	if c.code == "" {
		return false
	}
	known, ok := supportedCurrencies[c.code]
	return ok && known.exponent == c.exponent
}

func (c Currency) Code() string {
	return c.code
}

// Exponent is the ISO 4217 minor-unit exponent (2 for USD/CAD, 0 for JPY).
func (c Currency) Exponent() int {
	return c.exponent
}

func (c Currency) Equal(other Currency) bool {
	return c.Valid() && other.Valid() && c.code == other.code && c.exponent == other.exponent
}

func (c Currency) String() string {
	if !c.Valid() {
		return "<invalid-currency>"
	}
	return c.code
}

// MinorUnitsPerMajor is 10^exponent (100 for USD/CAD, 1 for JPY).
func (c Currency) MinorUnitsPerMajor() (int64, error) {
	if !c.Valid() {
		return 0, errors.New("invalid currency")
	}
	if c.exponent < 0 || c.exponent > 6 {
		return 0, fmt.Errorf("currency %s has unsupported exponent %d", c.code, c.exponent)
	}
	var f int64 = 1
	for i := 0; i < c.exponent; i++ {
		f *= 10
	}
	return f, nil
}

// MicrosPerMinorUnit converts one ISO minor unit into the ledger's 1e-6 major
// unit scale. For USD/CAD (exp 2) this is 10_000; for a zero-decimal currency
// it is 1_000_000 — never a silent *100.
func (c Currency) MicrosPerMinorUnit() (int64, error) {
	if !c.Valid() {
		return 0, errors.New("invalid currency")
	}
	if c.exponent < 0 || c.exponent > ledgerDecimalPlaces {
		return 0, fmt.Errorf("currency %s exponent %d exceeds ledger scale", c.code, c.exponent)
	}
	var f int64 = 1
	for i := 0; i < ledgerDecimalPlaces-c.exponent; i++ {
		f *= 10
	}
	return f, nil
}

// MajorToMinor converts a major-unit float (e.g. 12.34 CAD) into ISO minor
// units using this currency's exponent — never a hardcoded *100.
func (c Currency) MajorToMinor(major float64) (int64, error) {
	factor, err := c.MinorUnitsPerMajor()
	if err != nil {
		return 0, err
	}
	// math is imported by callers; keep conversion local without importing math
	// here would create a cycle risk with money helpers. Use integer-safe path:
	// scale then round half away from zero via float only at the edge.
	scaled := major * float64(factor)
	if scaled >= 0 {
		return int64(scaled + 0.5), nil
	}
	return int64(scaled - 0.5), nil
}

// ---------------------------------------------------------------------------
// MinorAmount: integer minor units bound to a Currency. Arithmetic that would
// mix two currencies is refused; there is no bare-int Add that forgets currency.
// ---------------------------------------------------------------------------

var (
	errCurrencyMismatch   = errors.New("currency mismatch: refusing cross-currency arithmetic")
	errInvalidCurrency    = errors.New("invalid currency")
	errSettlementCurrency = errors.New("settlement currency is not configured")
)

// MinorAmount is an amount in ISO minor units (cents for USD/CAD) tagged with
// its Currency. Construct only via NewMinorAmount.
type MinorAmount struct {
	amount   int64
	currency Currency
}

// NewMinorAmount binds amount and currency. Invalid currency fails closed.
func NewMinorAmount(amount int64, currency Currency) (MinorAmount, error) {
	if !currency.Valid() {
		return MinorAmount{}, errInvalidCurrency
	}
	return MinorAmount{amount: amount, currency: currency}, nil
}

// MustMinorAmount panics on invalid currency; for tests and known-good paths.
func MustMinorAmount(amount int64, currency Currency) MinorAmount {
	m, err := NewMinorAmount(amount, currency)
	if err != nil {
		panic(err)
	}
	return m
}

func (m MinorAmount) Amount() int64      { return m.amount }
func (m MinorAmount) Currency() Currency { return m.currency }
func (m MinorAmount) Valid() bool        { return m.currency.Valid() }

// Add returns m+other, or errCurrencyMismatch when currencies differ.
func (m MinorAmount) Add(other MinorAmount) (MinorAmount, error) {
	if !m.Valid() || !other.Valid() {
		return MinorAmount{}, errInvalidCurrency
	}
	if !m.currency.Equal(other.currency) {
		return MinorAmount{}, fmt.Errorf("%w: %s vs %s", errCurrencyMismatch, m.currency, other.currency)
	}
	sum := m.amount + other.amount
	// Overflow check.
	if (other.amount > 0 && sum < m.amount) || (other.amount < 0 && sum > m.amount) {
		return MinorAmount{}, errors.New("minor-amount add overflow")
	}
	return MinorAmount{amount: sum, currency: m.currency}, nil
}

// Sub returns m-other, or errCurrencyMismatch when currencies differ.
func (m MinorAmount) Sub(other MinorAmount) (MinorAmount, error) {
	neg, err := NewMinorAmount(-other.amount, other.currency)
	if err != nil {
		return MinorAmount{}, err
	}
	return m.Add(neg)
}

// SameCurrency reports whether both amounts share a valid currency.
func (m MinorAmount) SameCurrency(other MinorAmount) bool {
	return m.Valid() && other.Valid() && m.currency.Equal(other.currency)
}

// RequireCurrency refuses when m is not denominated in want.
func (m MinorAmount) RequireCurrency(want Currency) error {
	if !m.Valid() {
		return errInvalidCurrency
	}
	if !want.Valid() {
		return errInvalidCurrency
	}
	if !m.currency.Equal(want) {
		return fmt.Errorf("%w: have %s want %s", errCurrencyMismatch, m.currency, want)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Process-wide settlement currency (loaded once at boot).
// ---------------------------------------------------------------------------

var (
	settlementMu       sync.Mutex
	settlementLoaded   atomic.Bool
	settlementCurrency Currency
)

// LoadSettlementCurrencyFromEnv reads MERC_SETTLEMENT_CURRENCY, validates it,
// and installs it as the process settlement currency. Empty or unsupported
// values fail closed — there is no silent default to usd.
func LoadSettlementCurrencyFromEnv() (Currency, error) {
	raw := strings.TrimSpace(os.Getenv(settlementCurrencyEnv))
	if raw == "" {
		return Currency{}, fmt.Errorf("%s is required; refusing to start without an explicit settlement currency", settlementCurrencyEnv)
	}
	c, err := ParseCurrency(raw)
	if err != nil {
		return Currency{}, fmt.Errorf("%s=%q is not a supported settlement currency: %w", settlementCurrencyEnv, raw, err)
	}
	setSettlementCurrency(c)
	return c, nil
}

// setSettlementCurrency installs c as the process settlement currency.
// Exported only via test helper below.
func setSettlementCurrency(c Currency) {
	settlementMu.Lock()
	defer settlementMu.Unlock()
	settlementCurrency = c
	settlementLoaded.Store(true)
}

// SettlementCurrency returns the configured settlement currency, or an error
// when boot has not loaded one.
func SettlementCurrency() (Currency, error) {
	if !settlementLoaded.Load() {
		return Currency{}, errSettlementCurrency
	}
	settlementMu.Lock()
	defer settlementMu.Unlock()
	if !settlementCurrency.Valid() {
		return Currency{}, errSettlementCurrency
	}
	return settlementCurrency, nil
}

// MustSettlementCurrency returns the configured settlement currency or panics.
// Use only on paths that cannot usefully recover (after boot has loaded it).
func MustSettlementCurrency() Currency {
	c, err := SettlementCurrency()
	if err != nil {
		panic(err)
	}
	return c
}

// SettlementCurrencyCode is the lowercase ISO code, or "" when unset.
func SettlementCurrencyCode() string {
	c, err := SettlementCurrency()
	if err != nil {
		return ""
	}
	return c.Code()
}

// RequireSettlementCurrency returns an error when raw is not the configured
// settlement currency (after normalising case/whitespace).
func RequireSettlementCurrency(raw string) error {
	want, err := SettlementCurrency()
	if err != nil {
		return err
	}
	got, err := ParseCurrency(raw)
	if err != nil {
		return err
	}
	if !got.Equal(want) {
		return fmt.Errorf("%w: have %s want settlement currency %s", errCurrencyMismatch, got, want)
	}
	return nil
}

// IsSettlementCurrency reports whether raw matches the configured settlement
// currency. Unconfigured settlement or unsupported raw yields false.
func IsSettlementCurrency(raw string) bool {
	return RequireSettlementCurrency(raw) == nil
}

// resetSettlementCurrencyForTest clears the process settlement currency so
// boot-refusal tests can observe the unset state. Not for production use.
func resetSettlementCurrencyForTest() {
	settlementMu.Lock()
	defer settlementMu.Unlock()
	settlementCurrency = Currency{}
	settlementLoaded.Store(false)
}

// installSettlementCurrencyForTest sets the process currency for a single test
// and restores the previous value on cleanup.
func installSettlementCurrencyForTest(t interface {
	Helper()
	Cleanup(func())
}, code string) Currency {
	t.Helper()
	c, err := ParseCurrency(code)
	if err != nil {
		panic(err)
	}
	settlementMu.Lock()
	prev := settlementCurrency
	prevLoaded := settlementLoaded.Load()
	settlementMu.Unlock()

	setSettlementCurrency(c)
	t.Cleanup(func() {
		if prevLoaded {
			setSettlementCurrency(prev)
		} else {
			resetSettlementCurrencyForTest()
		}
	})
	return c
}
