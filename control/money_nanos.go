package main

import (
	"errors"
	"fmt"
	"math"
	"math/big"
)

// Exact economic value, in integer nano-major-units, bound to a currency.
//
// This exists because rounding economic authority to micro-USD before the
// transaction is large enough to round safely has now produced FOUR money
// defects, each arriving through arithmetic rather than through policy:
//
//	the LoRA compute floor truncating to zero on small quotes
//	the supplier share collapsing to 0.8% on a three-row job
//	the supplier payout rounding to exactly zero between 5 and 99 units
//	admission refusing every small job because a rounded per-task payout,
//	  divided back out into an hourly rate, falls under the hourly ceiling
//	  the same catalogue derived — 0.102978 against 0.104733, a 1.676% gap
//	  that is entirely one lost micro-USD
//
// The fourth is the one that made the precision layer unavoidable. Comparing the
// two ROUNDED numbers instead would have removed the false rejection and left the
// supplier ~1.7% short of the continuous floor they were promised — a quieter
// version of the same defect, which is why the obvious fix is the wrong one.
//
// The hierarchy this establishes, and the only place rounding is allowed:
//
//	economic planning     exact nano-major-units          <- here
//	internal accrual      nanos with carried remainder    <- here
//	ledger presentation   micro-major-units               round, carry the rest
//	provider collection   aggregated minor units (cents)  round, aggregated
//	bank payout           aggregated minor units          round, aggregated
//
// Round only when crossing into a lower-precision EXTERNAL domain, and never
// round something that is about to be compared against something unrounded.
//
// int64 nanos holds ±9.22e18 nanos = ±$9.22 billion in a single amount, which is
// more than any one execution contract can be. Intermediate products are computed
// in big.Int because a rate times a duration overflows int64 long before either
// operand is unreasonable.

// NanosPerMajorUnit is 1e9: one nano-USD is $0.000000001.
const NanosPerMajorUnit int64 = 1_000_000_000

// NanosPerMicro is the ledger's presentation granularity, in nanos.
const NanosPerMicro int64 = 1_000

// NanosPerHour converts an hourly rate to a per-nanosecond one.
const nanosecondsPerHour int64 = 3_600 * 1_000_000_000

// economicRoundingPolicy is bound into every plan and receipt that used this
// authority. A plan computed under one policy must never be re-read as though it
// had been computed under another — the whole point of the migration is that old
// rows keep their own arithmetic.
const economicRoundingPolicy = "economic-nanos-v1"

// MoneyNanos is an exact amount in one currency.
//
// The currency travels WITH the amount rather than beside it, because every
// money defect this tree has recorded began with two numbers that looked
// comparable and were not.
type MoneyNanos struct {
	Currency Currency `json:"currency"`
	Nanos    int64    `json:"nanos"`
}

// Rates, durations and quantities are distinct types on purpose. The admission
// bug compared a USD/hour against a USD/task without an explicit conversion, and
// the compiler could not object because both were float64.
type (
	// NanoUSDPerHour is a supplier's hourly floor or an admission ceiling.
	NanoUSDPerHour int64
	// NanoUSDPerThousandUnits is a catalogue price expressed per 1,000 units.
	NanoUSDPerThousandUnits int64
	// NanoMajorPerMillionTokens is a currency-bound realtime rate expressed as
	// nano-major-units per 1,000,000 tokens. The currency travels on the result;
	// a rate is never compared or added directly to another currency's rate.
	NanoMajorPerMillionTokens int64
	// NanoWorkUnits is billable units times 1e9, so a fraction of a unit stays
	// exact. It replaced an integer WorkUnits, which is not a widening for its own
	// sake: units are NOT whole. The input-side settlement formula is
	// max(records, bytes/4), and a 233-byte three-record corpus is 58.25 units, so
	// an integer type forced a ceil() somewhere — and every ceil() charged the
	// buyer's supplier floor for a fraction of a unit nobody bought. 1.3% on that
	// corpus, and 100% of the price on any job below a single unit.
	NanoWorkUnits int64
	// DurationNanos is elapsed or predicted time in nanoseconds.
	DurationNanos int64
	// UnitsPerSecond is governed throughput, in units per second, times 1e9 so it
	// stays an integer. A float throughput reintroduces exactly what this file
	// exists to remove.
	NanoUnitsPerSecond int64
)

var (
	errMoneyCurrencyMismatch = errors.New("money amounts are in different currencies")
	errMoneyOverflow         = errors.New("exact money arithmetic overflowed int64 nanos")
)

// NewMoneyNanos is the only constructor. It refuses a zero-value currency,
// because an amount whose currency is unset compares equal to nothing safely.
func NewMoneyNanos(c Currency, nanos int64) (MoneyNanos, error) {
	if c.Code() == "" {
		return MoneyNanos{}, errors.New("an exact amount must name its currency")
	}
	return MoneyNanos{Currency: c, Nanos: nanos}, nil
}

// MoneyNanosFromUSDFloat converts a legacy float64 amount at the boundary.
//
// Deliberately named for what it is. New authority code must not call it: it is
// the migration seam for values that already exist as float64 in frozen plans and
// on the wire, and every use is a place where exactness was lost before this
// function was reached, not by it.
//
// Rounds half away from zero, so a legacy value sitting exactly between two nanos
// does not drift toward zero and silently shave supplier entitlement.
func MoneyNanosFromUSDFloat(c Currency, usd float64) (MoneyNanos, error) {
	if !finiteNonNegative(math.Abs(usd)) {
		return MoneyNanos{}, fmt.Errorf("cannot convert non-finite %v to exact nanos", usd)
	}
	scaled := usd * float64(NanosPerMajorUnit)
	if scaled > math.MaxInt64 || scaled < math.MinInt64 {
		return MoneyNanos{}, errMoneyOverflow
	}
	return NewMoneyNanos(c, int64(math.Round(scaled)))
}

// USDFloat is for DISPLAY and for legacy projections only. It is lossy by
// construction and must never feed a comparison.
func (m MoneyNanos) USDFloat() float64 {
	return float64(m.Nanos) / float64(NanosPerMajorUnit)
}

func (m MoneyNanos) IsZero() bool     { return m.Nanos == 0 }
func (m MoneyNanos) IsPositive() bool { return m.Nanos > 0 }

func (m MoneyNanos) String() string {
	return fmt.Sprintf("%d nano-%s", m.Nanos, m.Currency.Code())
}

// sameCurrency refuses arithmetic across currencies rather than converting.
func (m MoneyNanos) sameCurrency(other MoneyNanos) error {
	if m.Currency.Code() != other.Currency.Code() {
		return fmt.Errorf("%w: %s and %s",
			errMoneyCurrencyMismatch, m.Currency.Code(), other.Currency.Code())
	}
	return nil
}

// Add and Sub are checked: silently wrapping an int64 is how a conservation
// invariant reports that value was created.
func (m MoneyNanos) Add(other MoneyNanos) (MoneyNanos, error) {
	if err := m.sameCurrency(other); err != nil {
		return MoneyNanos{}, err
	}
	sum := m.Nanos + other.Nanos
	if (other.Nanos > 0 && sum < m.Nanos) || (other.Nanos < 0 && sum > m.Nanos) {
		return MoneyNanos{}, errMoneyOverflow
	}
	return MoneyNanos{Currency: m.Currency, Nanos: sum}, nil
}

func (m MoneyNanos) Sub(other MoneyNanos) (MoneyNanos, error) {
	if err := m.sameCurrency(other); err != nil {
		return MoneyNanos{}, err
	}
	diff := m.Nanos - other.Nanos
	if (other.Nanos < 0 && diff < m.Nanos) || (other.Nanos > 0 && diff > m.Nanos) {
		return MoneyNanos{}, errMoneyOverflow
	}
	return MoneyNanos{Currency: m.Currency, Nanos: diff}, nil
}

// AtLeast is the admission comparison: exact, same currency, no rounding, no
// reconstruction through a rate.
func (m MoneyNanos) AtLeast(floor MoneyNanos) (bool, error) {
	if err := m.sameCurrency(floor); err != nil {
		return false, err
	}
	return m.Nanos >= floor.Nanos, nil
}

// AtMost is the buyer-ceiling comparison, in the same terms.
func (m MoneyNanos) AtMost(ceiling MoneyNanos) (bool, error) {
	if err := m.sameCurrency(ceiling); err != nil {
		return false, err
	}
	return m.Nanos <= ceiling.Nanos, nil
}

// mulDiv computes a*b/d in arbitrary precision and returns an int64, rounding in
// the named direction. Every rate conversion in this file goes through it, so
// there is exactly one place where a product can overflow or a division can round
// the wrong way.
func mulDiv(a, b, d int64, up bool) (int64, error) {
	if d == 0 {
		return 0, errors.New("exact money arithmetic divided by zero")
	}
	product := new(big.Int).Mul(big.NewInt(a), big.NewInt(b))
	divisor := big.NewInt(d)
	quotient, remainder := new(big.Int).QuoRem(product, divisor, new(big.Int))
	if remainder.Sign() != 0 && up == (product.Sign() == divisor.Sign()) {
		quotient.Add(quotient, big.NewInt(1))
	}
	if !quotient.IsInt64() {
		return 0, errMoneyOverflow
	}
	return quotient.Int64(), nil
}

// RequiredTaskNanosFromHourlyFloor converts a supplier's HOURLY minimum into the
// exact per-task amount that satisfies it, rounding UP.
//
// Up, always. Rounding a supplier's floor down means admitting a worker at a rate
// that does not clear the minimum they set, by an amount too small for them to
// notice per task and not too small to notice per month.
//
// This is the ONE canonical derivation. The defect this file exists for came from
// maintaining two: a ceiling derived forward from the catalogue in continuous
// dollars, and a gross reconstructed backward from a rounded payout. Reverse
// reconstruction is not offered here at all.
func RequiredTaskNanosFromHourlyFloor(
	c Currency, floor NanoUSDPerHour, predicted DurationNanos,
) (MoneyNanos, error) {
	if floor < 0 {
		return MoneyNanos{}, errors.New("an hourly floor cannot be negative")
	}
	if predicted < 0 {
		return MoneyNanos{}, errors.New("a predicted duration cannot be negative")
	}
	nanos, err := mulDiv(int64(floor), int64(predicted), nanosecondsPerHour, true)
	if err != nil {
		return MoneyNanos{}, err
	}
	return NewMoneyNanos(c, nanos)
}

// RequiredTaskNanosFromThroughput is the unit-based derivation of the same floor,
// for a benchmark authority that measures units per second rather than time.
//
//	units / (units per second) = seconds, then seconds x hourly floor
//
// Kept beside the duration form because a caller has one or the other, never
// both — and a deployment must pick ONE as authority rather than treating the
// pair as independent, which is how the two figures drifted apart before.
func RequiredTaskNanosFromThroughput(
	c Currency, floor NanoUSDPerHour, units NanoWorkUnits, throughput NanoUnitsPerSecond,
) (MoneyNanos, error) {
	if throughput <= 0 {
		return MoneyNanos{}, errors.New(
			"cannot derive a task floor from a non-positive governed throughput")
	}
	if units < 0 {
		return MoneyNanos{}, errors.New("work units cannot be negative")
	}
	// ONE division, at the end.
	//
	// This was two steps — divide by throughput, then scale to nanoseconds — and
	// the intermediate was an int64 count of SECONDS. Every task shorter than a
	// second truncated to one second, and the floor it produced was the whole
	// hourly ceiling over 3,600 no matter how little work the task held. On the
	// three-record embed fixture that is 29,093 nanos against a true 1,031, and it
	// is the bulk of the "30x gap between quote and catalogue" the programme
	// ledger recorded as a pricing disagreement. It was an integer division.
	//
	// units(nano) * 1e9 * 1e9 / (throughput(nano)) is the duration in nanoseconds.
	// The 1e18 numerator is formed inside mulDiv, in big.Int, so nothing overflows
	// on the way; the surviving int64 is the duration itself.
	durationNanos, err := mulDiv(
		int64(units), NanosPerMajorUnit, int64(throughput), true,
	)
	if err != nil {
		return MoneyNanos{}, err
	}
	return RequiredTaskNanosFromHourlyFloor(c, floor, DurationNanos(durationNanos))
}

// NanoWorkUnitsFromFloat converts a fractional unit count at the boundary.
//
// Named for what it is, like MoneyNanosFromUSDFloat: the unit formula still
// produces a float64, and this is where that float stops being one. Rounds half
// away from zero so a value sitting exactly between two nano-units does not drift
// toward zero.
func NanoWorkUnitsFromFloat(units float64) NanoWorkUnits {
	if math.IsNaN(units) || math.IsInf(units, 0) || units <= 0 {
		return 0
	}
	scaled := units * float64(NanosPerMajorUnit)
	if scaled > math.MaxInt64 {
		// Zero, not a clamp. Clamping would hand the money path a plausible-looking
		// unit count that is not the one the buyer submitted; zero is refused by
		// exactTaskEconomics and falls back to the legacy float derivation, which is
		// the fail-closed direction.
		return 0
	}
	return NanoWorkUnits(math.Round(scaled))
}

// nanosPer1KFromFloat converts a catalogue price per 1,000 units into nanos.
//
// Catalogue prices are published at eight decimal places (ceilPricePer1K), and
// eight decimals is exactly representable in nanos, so this conversion is lossless
// for every price the schedule can hold.
func nanosPer1KFromFloat(pricePer1K float64) NanoUSDPerThousandUnits {
	if math.IsNaN(pricePer1K) || math.IsInf(pricePer1K, 0) || pricePer1K <= 0 {
		return 0
	}
	return NanoUSDPerThousandUnits(math.Round(pricePer1K * float64(NanosPerMajorUnit)))
}

// nanoRatePerMillionFromFloat is the legacy realtime-profile/offer boundary.
// Once converted, token counts are multiplied before division in big.Int and no
// economic comparison returns to float64.
func nanoRatePerMillionFromFloat(rate float64) (NanoMajorPerMillionTokens, error) {
	if math.IsNaN(rate) || math.IsInf(rate, 0) || rate < 0 {
		return 0, errors.New("a realtime token rate must be finite and non-negative")
	}
	if rate*float64(NanosPerMajorUnit) > math.MaxInt64 {
		return 0, errMoneyOverflow
	}
	return NanoMajorPerMillionTokens(math.Round(rate * float64(NanosPerMajorUnit))), nil
}

func realtimeTokenChargeNanos(
	c Currency,
	promptTokens, completionTokens int64,
	inputRate, outputRate NanoMajorPerMillionTokens,
	roundSupplierUp bool,
) (MoneyNanos, error) {
	if promptTokens < 0 || completionTokens < 0 || inputRate < 0 || outputRate < 0 {
		return MoneyNanos{}, errors.New("realtime tokens and rates must be non-negative")
	}
	input, err := mulDiv(int64(inputRate), promptTokens, 1_000_000, roundSupplierUp)
	if err != nil {
		return MoneyNanos{}, err
	}
	output, err := mulDiv(int64(outputRate), completionTokens, 1_000_000, roundSupplierUp)
	if err != nil {
		return MoneyNanos{}, err
	}
	inputMoney, err := NewMoneyNanos(c, input)
	if err != nil {
		return MoneyNanos{}, err
	}
	outputMoney, err := NewMoneyNanos(c, output)
	if err != nil {
		return MoneyNanos{}, err
	}
	return inputMoney.Add(outputMoney)
}

// BuyerRealtimeTokenChargeNanos rounds each exact token-class product down.
// This is the buyer direction: a fraction of a nano is never charged as work.
func BuyerRealtimeTokenChargeNanos(
	c Currency, promptTokens, completionTokens int64,
	inputRate, outputRate NanoMajorPerMillionTokens,
) (MoneyNanos, error) {
	return realtimeTokenChargeNanos(c, promptTokens, completionTokens, inputRate, outputRate, false)
}

// SupplierRealtimeTokenEntitlementNanos rounds each exact token-class product
// up. This is the supplier direction: a positive entitlement is never shaved to
// zero before input and output are combined.
func SupplierRealtimeTokenEntitlementNanos(
	c Currency, promptTokens, completionTokens int64,
	inputRate, outputRate NanoMajorPerMillionTokens,
) (MoneyNanos, error) {
	return realtimeTokenChargeNanos(c, promptTokens, completionTokens, inputRate, outputRate, true)
}

// LedgerMicrosFromNanos projects one complete exact amount to the legacy ledger
// once, rounding half away from zero. It never rounds input and output classes
// separately. A positive sub-micro amount retains the historical one-micro
// minimum at this external precision boundary.
func LedgerMicrosFromNanos(amount MoneyNanos) (int64, error) {
	if amount.Currency.Code() == "" || amount.Nanos < 0 {
		return 0, errors.New("ledger projection requires non-negative currency-bound nanos")
	}
	whole := amount.Nanos / NanosPerMicro
	remainder := amount.Nanos % NanosPerMicro
	if remainder*2 >= NanosPerMicro {
		whole++
	}
	if whole == 0 && amount.Nanos > 0 {
		whole = 1
	}
	return whole, nil
}

// CatalogueGrossNanos is the buyer side of the ONE derivation: a catalogue price
// per 1,000 units times the exact fractional units delivered.
//
//	gross = price x units / 1000
//
// Rounds DOWN, because the buyer's direction is down: rounding a charge up by a
// fraction of a nano is charging for work not delivered, and the accepted ceiling
// is a promise about the maximum.
//
// This replaces the micro-USD float that used to freeze the same quantity. On the
// three-record fixture the exact gross is 1,436 nanos and roundUSD made it 1,000 —
// a 30.4% haircut, taken before the supplier's share was applied to it.
func CatalogueGrossNanos(
	c Currency, price NanoUSDPerThousandUnits, units NanoWorkUnits,
) (MoneyNanos, error) {
	if price < 0 {
		return MoneyNanos{}, errors.New("a catalogue price cannot be negative")
	}
	if units < 0 {
		return MoneyNanos{}, errors.New("work units cannot be negative")
	}
	// price is nanos per 1,000 units; units carries a 1e9 scale factor. So the
	// divisor is 1,000 x 1e9 and the product is formed in big.Int.
	nanos, err := mulDiv(int64(price), int64(units), 1_000*NanosPerMajorUnit, false)
	if err != nil {
		return MoneyNanos{}, err
	}
	return NewMoneyNanos(c, nanos)
}

// SupplierEntitlementNanos is the supplier side of the same derivation: an
// explicit share of the exact buyer gross.
//
// Rounds UP, opposite to the buyer, for the reason every direction in this file is
// chosen: a positive-but-tiny entitlement must never be shaved by a rounding step.
//
// The share is converted to a nano-fraction first so the multiplication never
// passes through float64 — a float share times a float amount is precisely the
// arithmetic this file exists to remove.
//
// Because the floor a job must clear and the entitlement it grants are BOTH this
// function applied to the same gross, admission compares a quantity against
// itself. That is the point: it is an identity, not a tolerance, and it holds at
// every job size rather than only above the size where rounding stops mattering.
func SupplierEntitlementNanos(gross MoneyNanos, share float64) (MoneyNanos, error) {
	if math.IsNaN(share) || math.IsInf(share, 0) || share <= 0 || share > 1 {
		return MoneyNanos{}, fmt.Errorf("supplier share %v is not inside (0,1]", share)
	}
	if gross.Nanos < 0 {
		return MoneyNanos{}, errors.New("a buyer gross cannot be negative")
	}
	shareNanos := int64(math.Round(share * float64(NanosPerMajorUnit)))
	nanos, err := mulDiv(gross.Nanos, shareNanos, NanosPerMajorUnit, true)
	if err != nil {
		return MoneyNanos{}, err
	}
	if nanos > gross.Nanos {
		// Only reachable when share rounds to exactly 1e9 and the round-up adds a
		// nano. The supplier is never entitled to more than the buyer was charged.
		nanos = gross.Nanos
	}
	return NewMoneyNanos(gross.Currency, nanos)
}

// RemainderCarry is the accrual ledger's fractional memory.
//
// Posting to a micro-USD ledger loses up to 999 nanos every time. Doing that
// per task, thousands of times, is how sub-cent supplier earnings vanished before
// — and the fix that was applied one precision layer up is applied again here:
// carry what could not be posted, and post it once it accumulates to a whole
// unit.
type RemainderCarry struct {
	Currency         Currency `json:"currency"`
	RemainderNanos   int64    `json:"remainder_nanos"`
	PostedMicros     int64    `json:"posted_micros"`
	rrRoundingPolicy string
}

// NewRemainderCarry starts an accrual with a stated opening remainder.
func NewRemainderCarry(c Currency, openingRemainderNanos int64) (RemainderCarry, error) {
	if c.Code() == "" {
		return RemainderCarry{}, errors.New("a remainder carry must name its currency")
	}
	if openingRemainderNanos < 0 || openingRemainderNanos >= NanosPerMicro {
		return RemainderCarry{}, fmt.Errorf(
			"opening remainder %d is not inside [0,%d); a remainder that large is a "+
				"whole micro that was never posted",
			openingRemainderNanos, NanosPerMicro)
	}
	return RemainderCarry{
		Currency: c, RemainderNanos: openingRemainderNanos,
		rrRoundingPolicy: economicRoundingPolicy,
	}, nil
}

// Accrue adds an exact amount and reports the whole micro-units now postable.
//
// The invariant, asserted by the caller and by the tests:
//
//	every exact nano accrued
//	  = every micro posted, in nanos
//	  + the remainder still carried
//
// Nothing is lost and nothing is created. That is the whole contract.
func (r *RemainderCarry) Accrue(amount MoneyNanos) (postMicros int64, err error) {
	if amount.Currency.Code() != r.Currency.Code() {
		return 0, fmt.Errorf("%w: accruing %s into a %s carry",
			errMoneyCurrencyMismatch, amount.Currency.Code(), r.Currency.Code())
	}
	if amount.Nanos < 0 {
		return 0, errors.New("a remainder carry accrues entitlement, not reversals")
	}
	total := r.RemainderNanos + amount.Nanos
	if total < r.RemainderNanos {
		return 0, errMoneyOverflow
	}
	postMicros = total / NanosPerMicro
	r.RemainderNanos = total % NanosPerMicro
	r.PostedMicros += postMicros
	return postMicros, nil
}

// ExactAccrued is everything the carry has ever seen, in nanos: what was posted
// plus what is still held. This is the left-hand side of the conservation check.
func (r RemainderCarry) ExactAccrued() int64 {
	return r.PostedMicros*NanosPerMicro + r.RemainderNanos
}

// RoundingPolicy names the arithmetic this carry used, for the receipt.
func (r RemainderCarry) RoundingPolicy() string {
	if r.rrRoundingPolicy == "" {
		return economicRoundingPolicy
	}
	return r.rrRoundingPolicy
}
