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
	// WorkUnits is billable units — rows embedded, tokens generated.
	WorkUnits int64
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
	c Currency, floor NanoUSDPerHour, units WorkUnits, throughput NanoUnitsPerSecond,
) (MoneyNanos, error) {
	if throughput <= 0 {
		return MoneyNanos{}, errors.New(
			"cannot derive a task floor from a non-positive governed throughput")
	}
	if units < 0 {
		return MoneyNanos{}, errors.New("work units cannot be negative")
	}
	// seconds = units / (throughput/1e9) = units * 1e9 / throughput, in nanoseconds
	// that is units * 1e9 * 1e9 / throughput — done in two checked steps so the
	// intermediate stays in big.Int rather than in an int64 that would overflow.
	secondsNanos, err := mulDiv(int64(units), NanosPerMajorUnit, int64(throughput), true)
	if err != nil {
		return MoneyNanos{}, err
	}
	durationNanos, err := mulDiv(secondsNanos, NanosPerMajorUnit, 1, true)
	if err != nil {
		return MoneyNanos{}, err
	}
	return RequiredTaskNanosFromHourlyFloor(c, floor, DurationNanos(durationNanos))
}

// TaskChargeNanosFromCataloguePrice is the buyer side: a catalogue price per
// 1,000 units times the units delivered, rounded DOWN.
//
// Down, because the buyer's direction is the opposite of the supplier's. Rounding
// a charge up by a fraction of a nano is charging for work not delivered, and the
// accepted ceiling is a promise about the maximum.
func TaskChargeNanosFromCataloguePrice(
	c Currency, price NanoUSDPerThousandUnits, units WorkUnits,
) (MoneyNanos, error) {
	if price < 0 || units < 0 {
		return MoneyNanos{}, errors.New("a catalogue charge needs a non-negative price and units")
	}
	nanos, err := mulDiv(int64(price), int64(units), 1_000, false)
	if err != nil {
		return MoneyNanos{}, err
	}
	return NewMoneyNanos(c, nanos)
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
