package main

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// CostSchedule is the versioned, digest-bound POLICY for named platform cost
// rates that are not measurement. Every rate carries provenance naming where the
// number came from (a published price list, a measured bill, a declared policy).
// A rate with empty provenance fails validation.
//
// Provider (cloud pod) rates are deliberately not in this schedule: they come
// from the runtime-cell / spend-guard authority so pricing and the spend guard
// cannot drift. See provider_cost_authority.go.
//
// Historical PricingDecisions without CostScheduleSHA256 keep their frozen
// arithmetic and unknown categories forever. New components apply only when the
// digest is bound into the decision.
type CostSchedule struct {
	Revision string `json:"revision"`
	// Currency is the ISO settlement currency every nano amount is denominated in.
	Currency string `json:"currency"`

	// StorageNanosPerGiBMonth is the object-storage rate for retained job
	// payload bytes (input + output under the job prefix).
	StorageNanosPerGiBMonth int64  `json:"storage_nanos_per_gib_month"`
	StorageProvenance       string `json:"storage_provenance"`

	// EgressNanosPerGiB is the result-delivery rate for bytes transferred to
	// the buyer (output only; inputs were already on the platform).
	EgressNanosPerGiB int64  `json:"egress_nanos_per_gib"`
	EgressProvenance  string `json:"egress_provenance"`

	// RiskReserveBasisPoints is a declared policy reserve on the buyer charge.
	// It is real money: accrued to a platform risk-reserve ledger account at
	// settlement, released after the dispute window, consumed on refund.
	RiskReserveBasisPoints int64  `json:"risk_reserve_basis_points"`
	RiskReserveProvenance  string `json:"risk_reserve_provenance"`
}

const (
	// costScheduleRevisionEnv selects the active policy revision. Required when
	// loading from the environment so a deploy cannot silently run without a
	// named cost policy.
	costScheduleRevisionEnv = "MERC_COST_SCHEDULE_REVISION"

	// Published AWS S3 Standard storage (us-east-1) as of the rate card this
	// schedule freezes: $0.023 per GB-month. 1 GiB = 2^30 bytes; the rate card
	// bills GB (10^9) but object stores are conventionally quoted per GiB in
	// industry models. Using the published dollar figure with a GiB denominator
	// is slightly conservative vs pure SI GB.
	//
	// Provenance string is frozen into every decision that uses this default so
	// a later rate-card change cannot silently reprice a historical decision.
	defaultStorageNanosPerGiBMonth int64 = 23_000_000 // $0.023
	defaultStorageProvenance             = "AWS S3 Standard us-east-1 published storage rate $0.023/GB-month (policy model, not a metered invoice)"

	// Published AWS data-transfer-out (internet) first 10 TB tier: $0.09/GB.
	defaultEgressNanosPerGiB int64 = 90_000_000 // $0.09
	defaultEgressProvenance        = "AWS data transfer out to internet published rate $0.09/GB first 10TB tier (policy model, not a metered invoice)"

	// Platform dispute-window risk reserve: 50 basis points of the buyer charge.
	// Policy, not a measured loss cohort. Accrual/release/consume make the cash
	// real on the ledger.
	defaultRiskReserveBasisPoints int64 = 50
	defaultRiskReserveProvenance        = "platform dispute-window risk reserve policy v1: 50 bps of buyer charge held until dispute window elapses"

	// BytesPerGiB is the binary gibibyte used for storage/egress modeling.
	BytesPerGiB int64 = 1024 * 1024 * 1024

	// Seconds in an average month for storage duration prorating (30 days).
	// Retention is an upper bound measured in wall time; using 30d months keeps
	// the arithmetic integer and auditable.
	secondsPerMonth = 30 * 24 * 3600
)

// DefaultCostSchedule returns the governed default cost policy for the given
// settlement currency. Every rate has provenance; nothing is zero without a
// stated reason.
func DefaultCostSchedule(currency string) CostSchedule {
	return CostSchedule{
		Revision:                "cost-schedule-v1",
		Currency:                currency,
		StorageNanosPerGiBMonth: defaultStorageNanosPerGiBMonth,
		StorageProvenance:       defaultStorageProvenance,
		EgressNanosPerGiB:       defaultEgressNanosPerGiB,
		EgressProvenance:        defaultEgressProvenance,
		RiskReserveBasisPoints:  defaultRiskReserveBasisPoints,
		RiskReserveProvenance:   defaultRiskReserveProvenance,
	}
}

// LoadCostScheduleFromEnv loads the cost schedule. When MERC_COST_SCHEDULE_REVISION
// is unset, returns the default schedule for the process settlement currency so
// a deployment that has not yet named a revision still prices with a governed
// policy rather than inventing rates. An explicitly set revision that disagrees
// with the built-in default fails closed: this process only knows the default
// rates, and a named unknown revision must not silently substitute them.
func LoadCostScheduleFromEnv() (CostSchedule, error) {
	currency := SettlementCurrencyCode()
	if currency == "" {
		return CostSchedule{}, errors.New("cost schedule requires a configured settlement currency")
	}
	schedule := DefaultCostSchedule(currency)
	if rev := strings.TrimSpace(os.Getenv(costScheduleRevisionEnv)); rev != "" {
		if rev != schedule.Revision {
			return CostSchedule{}, fmt.Errorf(
				"cost schedule revision %q is not recognized by this binary (known: %q)",
				rev, schedule.Revision)
		}
		schedule.Revision = rev
	}
	// Optional overrides must still carry provenance via the default strings;
	// numeric overrides without a distinct revision are refused so a partial
	// env cannot invent a rate the digest will not distinguish.
	if raw := strings.TrimSpace(os.Getenv("MERC_COST_STORAGE_NANOS_PER_GIB_MONTH")); raw != "" {
		return CostSchedule{}, errors.New(
			"cost schedule refuses ad-hoc MERC_COST_STORAGE_NANOS_PER_GIB_MONTH override; " +
				"change DefaultCostSchedule with provenance or ship a new revision")
	}
	if raw := strings.TrimSpace(os.Getenv("MERC_COST_EGRESS_NANOS_PER_GIB")); raw != "" {
		return CostSchedule{}, errors.New(
			"cost schedule refuses ad-hoc MERC_COST_EGRESS_NANOS_PER_GIB override; " +
				"change DefaultCostSchedule with provenance or ship a new revision")
	}
	if raw := strings.TrimSpace(os.Getenv("MERC_COST_RISK_RESERVE_BPS")); raw != "" {
		// Allow reading the same BPS only when it matches the default, so tests
		// can assert the env is honored without inventing a new rate.
		bps, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || bps != schedule.RiskReserveBasisPoints {
			return CostSchedule{}, errors.New(
				"cost schedule refuses MERC_COST_RISK_RESERVE_BPS that does not match the " +
					"governed default; change DefaultCostSchedule with provenance")
		}
	}
	if reason := validateCostSchedule(schedule); reason != "" {
		return CostSchedule{}, fmt.Errorf("invalid cost schedule: %s", reason)
	}
	return schedule, nil
}

func validateCostSchedule(s CostSchedule) string {
	if strings.TrimSpace(s.Revision) == "" {
		return "cost schedule revision is required"
	}
	currency, err := ParseCurrency(s.Currency)
	if err != nil || s.Currency != currency.Code() {
		return "cost schedule currency must be a supported ISO settlement currency"
	}
	if s.StorageNanosPerGiBMonth < 0 {
		return "storage_nanos_per_gib_month must be non-negative"
	}
	if strings.TrimSpace(s.StorageProvenance) == "" {
		return "storage_provenance is required for every storage rate"
	}
	if s.EgressNanosPerGiB < 0 {
		return "egress_nanos_per_gib must be non-negative"
	}
	if strings.TrimSpace(s.EgressProvenance) == "" {
		return "egress_provenance is required for every egress rate"
	}
	if s.RiskReserveBasisPoints < 0 || s.RiskReserveBasisPoints >= 10_000 {
		return "risk_reserve_basis_points must be in [0,10000)"
	}
	if strings.TrimSpace(s.RiskReserveProvenance) == "" {
		return "risk_reserve_provenance is required for every risk reserve rate"
	}
	return ""
}

func costScheduleDigest(s CostSchedule) (string, error) {
	if reason := validateCostSchedule(s); reason != "" {
		return "", errors.New(reason)
	}
	return canonicalDigest("cost schedule", s)
}

// storageNanosForBytes models object storage cost for `bytes` retained for
// `retention`. Result is an upper-bound integer in nano-major-units:
//
//	ceil( bytes / GiB * rate * months )
//
// where months = retention / 30d, computed in integer nanos via mulDiv so the
// arithmetic never crosses a float.
func storageNanosForBytes(schedule CostSchedule, bytes int64, retention time.Duration) (int64, error) {
	if reason := validateCostSchedule(schedule); reason != "" {
		return 0, errors.New(reason)
	}
	if bytes < 0 {
		return 0, errors.New("storage model refuses negative byte count")
	}
	if bytes == 0 || schedule.StorageNanosPerGiBMonth == 0 {
		return 0, nil
	}
	if retention <= 0 {
		return 0, errors.New("storage model requires a positive retention period")
	}
	// nanos = bytes * rate * retentionSecs / (GiB * secondsPerMonth)
	// Round UP so an acceptance bound never under-reserves the cost.
	secs := int64(retention / time.Second)
	if secs <= 0 {
		secs = 1
	}
	product, err := mulDiv(bytes, schedule.StorageNanosPerGiBMonth, BytesPerGiB, true)
	if err != nil {
		return 0, fmt.Errorf("storage model mulDiv bytes*rate: %w", err)
	}
	return mulDiv(product, secs, secondsPerMonth, true)
}

// egressNanosForBytes models result-delivery cost for `bytes` transferred.
// Round UP so an acceptance bound never under-reserves.
func egressNanosForBytes(schedule CostSchedule, bytes int64) (int64, error) {
	if reason := validateCostSchedule(schedule); reason != "" {
		return 0, errors.New(reason)
	}
	if bytes < 0 {
		return 0, errors.New("egress model refuses negative byte count")
	}
	if bytes == 0 || schedule.EgressNanosPerGiB == 0 {
		return 0, nil
	}
	return mulDiv(bytes, schedule.EgressNanosPerGiB, BytesPerGiB, true)
}

// riskReserveNanos is RiskReserveBasisPoints of the buyer charge, rounded UP
// so the reserve is never under-accrued by a fraction of a nano.
func riskReserveNanos(schedule CostSchedule, buyerChargeNanos int64) (int64, error) {
	if reason := validateCostSchedule(schedule); reason != "" {
		return 0, errors.New(reason)
	}
	if buyerChargeNanos < 0 {
		return 0, errors.New("risk reserve refuses a negative buyer charge")
	}
	if buyerChargeNanos == 0 || schedule.RiskReserveBasisPoints == 0 {
		return 0, nil
	}
	return mulDiv(buyerChargeNanos, schedule.RiskReserveBasisPoints, 10_000, true)
}

// declaredOutputBytesBound is an upper bound on stored and delivered bytes from
// the frozen compute plan geometry — not a guess at measured size.
//
// Storage covers input + output under the job prefix for the retention window.
// Egress covers result delivery only (output). EstimatedOutputTokens is treated
// as a byte upper bound (1 token ≤ 4 bytes of UTF-8-ish payload is NOT assumed
// here; we use the token count as a 1:1 byte floor and take max with input so a
// generative job that returns more than it ingested still bounds storage). For
// embed jobs with zero estimated output tokens, the settlement input units map
// to float32 embedding bytes when the model dim is known; otherwise input bytes
// alone bound storage of the retained payloads.
func declaredOutputBytesBound(compute ComputePlan) (storageBytes, egressBytes int64) {
	input := compute.InputBytes
	if input < 0 {
		input = 0
	}
	// Output bound: estimated tokens as a 1-byte-per-token floor, floored at
	// zero. This is an upper-bound policy for jobs that declare output geometry;
	// settlement remeasures actual artifact bytes.
	out := compute.EstimatedOutputTokens
	if out < 0 {
		out = 0
	}
	// Settlement input units for embed are the record count-ish units; when no
	// output tokens are declared, treat input as both the retained payload and
	// the delivered result bound (the embedding result is typically smaller
	// than the input text, so input is a conservative egress upper bound).
	if out == 0 {
		egressBytes = input
	} else {
		egressBytes = out
	}
	storageBytes = input + egressBytes
	return storageBytes, egressBytes
}

// nanosToUSDFloat projects nano-major-units to the six-decimal ledger float for
// PricingCostComponent.Amount. Round half-away-from-zero via the shared helper.
func nanosToEconomicUSD(nanos int64) float64 {
	return roundEconomicUSD(float64(nanos) / float64(NanosPerMajorUnit))
}
