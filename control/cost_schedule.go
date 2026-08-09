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
// display arithmetic and unknown categories forever. Digest-only decisions
// predate the complete body and are explicitly unreplayable; current decisions
// bind both the digest and FrozenCostPolicySnapshot.
type CostSchedule struct {
	Revision string `json:"revision"`
	// ReferenceCurrency names the currency of the published source rates.
	// SettlementCurrency names the currency used by the converted nano rates.
	// Keeping both prevents a USD rate from being numerically relabeled CAD.
	ReferenceCurrency  string `json:"reference_currency"`
	SettlementCurrency string `json:"settlement_currency"`

	// StorageReferenceNanosPerGiBMonth is the exact published reference rate.
	// StorageNanosPerGiBMonth is its governed settlement-currency conversion.
	StorageReferenceNanosPerGiBMonth int64  `json:"storage_reference_nanos_per_gib_month"`
	StorageNanosPerGiBMonth          int64  `json:"storage_nanos_per_gib_month"`
	StorageProvenance                string `json:"storage_provenance"`

	// EgressReferenceNanosPerGiB is the exact published reference rate.
	// EgressNanosPerGiB is its governed settlement-currency conversion.
	EgressReferenceNanosPerGiB int64  `json:"egress_reference_nanos_per_gib"`
	EgressNanosPerGiB          int64  `json:"egress_nanos_per_gib"`
	EgressProvenance           string `json:"egress_provenance"`

	// RiskReserveBasisPoints is a declared policy reserve on the buyer charge.
	// It is real money: accrued to a platform risk-reserve ledger account at
	// settlement, released after the dispute window, consumed on refund.
	RiskReserveBasisPoints int64  `json:"risk_reserve_basis_points"`
	RiskReserveProvenance  string `json:"risk_reserve_provenance"`
}

const (
	frozenCostPolicySnapshotVersionV1 = 1
	frozenCostPolicySnapshotVersion   = frozenCostPolicySnapshotVersionV1
	// frozenCostPolicyV1RetentionPolicyRevision is historical replay authority,
	// not an alias for whichever retention policy new admissions use today. A
	// future retention-policy revision therefore requires a new frozen snapshot
	// version instead of making already-accepted v1 decisions unreadable.
	frozenCostPolicyV1RetentionPolicyRevision       = "job-object-retention-v1"
	costFXAuthorityVersion                          = 1
	costReferenceCurrency                           = "usd"
	costFXRateScale                           int64 = NanosPerMajorUnit
	costFXRoundingPolicy                            = "reference-cost-rate-times-fx-ceil-nanos-v1"
)

// CostFXAuthority is the batch cost lane's exact conversion from published USD
// rates into the PricingDecision settlement currency. It is cross-bound to the
// catalogue's governed FX identity, but remains lane-specific: unlike token
// charging, cost rates always round up before byte/duration prorating so a
// positive platform liability cannot disappear.
type CostFXAuthority struct {
	Version                    int     `json:"version"`
	ReferenceCurrency          string  `json:"reference_currency"`
	SettlementCurrency         string  `json:"settlement_currency"`
	ReferenceToSettlementRate  float64 `json:"reference_to_settlement_rate"`
	ReferenceToSettlementNanos int64   `json:"reference_to_settlement_nanos"`
	FXRevision                 string  `json:"fx_revision"`
	RoundingPolicy             string  `json:"rounding_policy"`
}

// FrozenCostPolicySnapshot is the complete cost-policy authority accepted by a
// distributed PricingDecision. CostScheduleSHA256 alone can detect that a
// caller supplied a different rate card, but it cannot reconstruct the old
// card after a deploy. Retention was even weaker: only today's environment was
// available. Freezing both authorities makes replay and byte settlement
// independent of process configuration.
type FrozenCostPolicySnapshot struct {
	Version int `json:"version"`

	Schedule       CostSchedule    `json:"schedule"`
	ScheduleSHA256 string          `json:"schedule_sha256"`
	FX             CostFXAuthority `json:"fx"`

	RetentionSeconds        int64  `json:"retention_seconds"`
	RetentionPolicyRevision string `json:"retention_policy_revision"`
	RetentionBasis          string `json:"retention_basis"`
}

const (
	// costScheduleRevisionEnv selects the active policy revision. Required when
	// loading from the environment so a deploy cannot silently run without a
	// named cost policy.
	costScheduleRevisionEnv = "MERC_COST_SCHEDULE_REVISION"
	costScheduleRevision    = "cost-schedule-v2"

	// Published AWS S3 Standard storage (us-east-1) as of the rate card this
	// schedule freezes: $0.023 per GB-month. 1 GiB = 2^30 bytes; the rate card
	// bills GB (10^9) but object stores are conventionally quoted per GiB in
	// industry models. Using the published dollar figure with a GiB denominator
	// is slightly conservative vs pure SI GB.
	//
	// Provenance string is frozen into every decision that uses this default so
	// a later rate-card change cannot silently reprice a historical decision.
	defaultStorageNanosPerGiBMonth int64 = 23_000_000 // $0.023
	defaultStorageProvenance             = "AWS S3 Standard us-east-1 published storage rate USD 0.023/GB-month (policy model, not a metered invoice)"

	// Published AWS data-transfer-out (internet) first 10 TB tier: $0.09/GB.
	defaultEgressNanosPerGiB int64 = 90_000_000 // $0.09
	defaultEgressProvenance        = "AWS data transfer out to internet published rate USD 0.09/GB first 10TB tier (policy model, not a metered invoice)"

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

// DefaultCostSchedule returns the governed USD reference policy and explicitly
// names the requested settlement currency. For USD, the settlement rates are
// exact identity. For any other currency they deliberately remain unresolved
// until applyCostScheduleFX receives a governed CostFXAuthority; they are never
// populated by relabeling the USD numbers.
func DefaultCostSchedule(settlementCurrency string) CostSchedule {
	schedule := CostSchedule{
		Revision:                         costScheduleRevision,
		ReferenceCurrency:                costReferenceCurrency,
		SettlementCurrency:               settlementCurrency,
		StorageReferenceNanosPerGiBMonth: defaultStorageNanosPerGiBMonth,
		StorageProvenance:                defaultStorageProvenance,
		EgressReferenceNanosPerGiB:       defaultEgressNanosPerGiB,
		EgressProvenance:                 defaultEgressProvenance,
		RiskReserveBasisPoints:           defaultRiskReserveBasisPoints,
		RiskReserveProvenance:            defaultRiskReserveProvenance,
	}
	if settlementCurrency == costReferenceCurrency {
		schedule.StorageNanosPerGiBMonth = schedule.StorageReferenceNanosPerGiBMonth
		schedule.EgressNanosPerGiB = schedule.EgressReferenceNanosPerGiB
	}
	return schedule
}

// LoadCostScheduleFromEnv loads the cost schedule. When MERC_COST_SCHEDULE_REVISION
// is unset, returns the default schedule for the process settlement currency so
// a deployment that has not yet named a revision still prices with a governed
// policy rather than inventing rates. An explicitly set revision that disagrees
// with the built-in default fails closed: this process only knows the default
// rates, and a named unknown revision must not silently substitute them.
func LoadCostScheduleFromEnv(fx CostFXAuthority) (CostSchedule, error) {
	currency := SettlementCurrencyCode()
	if currency == "" {
		return CostSchedule{}, errors.New("cost schedule requires a configured settlement currency")
	}
	settlement, err := ParseCurrency(currency)
	if err != nil {
		return CostSchedule{}, err
	}
	if err := validateCostFXAuthority(fx, settlement); err != nil {
		return CostSchedule{}, fmt.Errorf("cost schedule FX authority: %w", err)
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
	schedule, err = applyCostScheduleFX(schedule, fx)
	if err != nil {
		return CostSchedule{}, err
	}
	return schedule, nil
}

func validateCostFXAuthority(fx CostFXAuthority, settlement Currency) error {
	if !settlement.Valid() || fx.Version != costFXAuthorityVersion ||
		fx.ReferenceCurrency != costReferenceCurrency ||
		fx.SettlementCurrency != settlement.Code() ||
		fx.RoundingPolicy != costFXRoundingPolicy {
		return errors.New("cost FX authority has unsupported version, currency pair, or rounding policy")
	}
	if !finiteNonNegative(fx.ReferenceToSettlementRate) ||
		fx.ReferenceToSettlementRate <= 0 || fx.ReferenceToSettlementNanos <= 0 {
		return errors.New("cost FX authority lacks a finite positive rate")
	}
	exact, err := nanoRatePerMillionFromFloat(fx.ReferenceToSettlementRate)
	if err != nil || int64(exact) != fx.ReferenceToSettlementNanos {
		return errors.New("cost FX display rate disagrees with exact nano authority")
	}
	if strings.TrimSpace(fx.FXRevision) == "" || len(fx.FXRevision) > 128 ||
		strings.ContainsAny(fx.FXRevision, "\r\n\t") {
		return errors.New("cost FX authority lacks a valid governed revision")
	}
	if fx.ReferenceCurrency == fx.SettlementCurrency {
		if fx.ReferenceToSettlementRate != 1 ||
			fx.ReferenceToSettlementNanos != costFXRateScale ||
			fx.FXRevision != "identity-"+fx.ReferenceCurrency {
			return errors.New("same-currency cost FX authority is not exact identity")
		}
	} else if strings.HasPrefix(fx.FXRevision, "identity-") {
		return errors.New("cross-currency cost FX authority falsely claims identity")
	}
	return nil
}

// costFXAuthorityFromCatalogue closes the catalogue's governed float FX field
// into a 1e-9 exact factor. The catalogue remains the source of truth for the
// pair and revision; this snapshot makes later cost arithmetic integer-only.
func costFXAuthorityFromCatalogue(catalogue CataloguePriceAuthority) (CostFXAuthority, error) {
	if catalogue.ReferenceCurrency != costReferenceCurrency {
		return CostFXAuthority{}, fmt.Errorf(
			"cost policy reference currency %q is unsupported; published rates are %s",
			catalogue.ReferenceCurrency, costReferenceCurrency,
		)
	}
	settlement, err := ParseCurrency(catalogue.SettlementCurrency)
	if err != nil || settlement.Code() != catalogue.SettlementCurrency {
		return CostFXAuthority{}, errors.New("cost policy catalogue settlement currency is unsupported")
	}
	exact, err := nanoRatePerMillionFromFloat(catalogue.ReferenceToSettlementRate)
	if err != nil || exact <= 0 {
		return CostFXAuthority{}, errors.New("cost policy catalogue lacks an exact positive FX factor")
	}
	fx := CostFXAuthority{
		Version: costFXAuthorityVersion, ReferenceCurrency: catalogue.ReferenceCurrency,
		SettlementCurrency:         catalogue.SettlementCurrency,
		ReferenceToSettlementRate:  catalogue.ReferenceToSettlementRate,
		ReferenceToSettlementNanos: int64(exact), FXRevision: catalogue.FXRevision,
		RoundingPolicy: costFXRoundingPolicy,
	}
	if err := validateCostFXAuthority(fx, settlement); err != nil {
		return CostFXAuthority{}, err
	}
	return fx, nil
}

func validateCostFXCatalogueBinding(fx CostFXAuthority, catalogue CataloguePriceAuthority) error {
	want, err := costFXAuthorityFromCatalogue(catalogue)
	if err != nil {
		return err
	}
	if fx != want {
		return errors.New("frozen cost FX authority disagrees with catalogue FX authority")
	}
	return nil
}

func applyCostScheduleFX(schedule CostSchedule, fx CostFXAuthority) (CostSchedule, error) {
	settlement, err := ParseCurrency(schedule.SettlementCurrency)
	if err != nil {
		return CostSchedule{}, err
	}
	if err := validateCostFXAuthority(fx, settlement); err != nil {
		return CostSchedule{}, err
	}
	if schedule.ReferenceCurrency != fx.ReferenceCurrency ||
		schedule.SettlementCurrency != fx.SettlementCurrency {
		return CostSchedule{}, errors.New("cost schedule and FX authority currency pairs disagree")
	}
	storage, err := mulDiv(
		schedule.StorageReferenceNanosPerGiBMonth,
		fx.ReferenceToSettlementNanos, costFXRateScale, true,
	)
	if err != nil {
		return CostSchedule{}, fmt.Errorf("convert storage cost rate: %w", err)
	}
	egress, err := mulDiv(
		schedule.EgressReferenceNanosPerGiB,
		fx.ReferenceToSettlementNanos, costFXRateScale, true,
	)
	if err != nil {
		return CostSchedule{}, fmt.Errorf("convert egress cost rate: %w", err)
	}
	schedule.StorageNanosPerGiBMonth = storage
	schedule.EgressNanosPerGiB = egress
	if reason := validateCostSchedule(schedule); reason != "" {
		return CostSchedule{}, fmt.Errorf("invalid cost schedule: %s", reason)
	}
	return schedule, nil
}

func validateCostScheduleFXBinding(schedule CostSchedule, fx CostFXAuthority) error {
	want := schedule
	want.StorageNanosPerGiBMonth = 0
	want.EgressNanosPerGiB = 0
	want, err := applyCostScheduleFX(want, fx)
	if err != nil {
		return err
	}
	if schedule.StorageNanosPerGiBMonth != want.StorageNanosPerGiBMonth ||
		schedule.EgressNanosPerGiB != want.EgressNanosPerGiB {
		return errors.New("cost schedule settlement rates do not match reference rates and frozen FX")
	}
	return nil
}

func validateCostSchedule(s CostSchedule) string {
	if strings.TrimSpace(s.Revision) == "" {
		return "cost schedule revision is required"
	}
	reference, err := ParseCurrency(s.ReferenceCurrency)
	if err != nil || s.ReferenceCurrency != reference.Code() ||
		s.ReferenceCurrency != costReferenceCurrency {
		return "cost schedule reference currency must be canonical usd"
	}
	settlement, err := ParseCurrency(s.SettlementCurrency)
	if err != nil || s.SettlementCurrency != settlement.Code() {
		return "cost schedule settlement currency must be a supported canonical ISO currency"
	}
	if s.StorageReferenceNanosPerGiBMonth < 0 {
		return "storage_reference_nanos_per_gib_month must be non-negative"
	}
	if s.StorageNanosPerGiBMonth < 0 ||
		(s.StorageReferenceNanosPerGiBMonth > 0 && s.StorageNanosPerGiBMonth == 0) {
		return "storage_nanos_per_gib_month must be positive when the reference rate is positive"
	}
	if strings.TrimSpace(s.StorageProvenance) == "" {
		return "storage_provenance is required for every storage rate"
	}
	if s.EgressReferenceNanosPerGiB < 0 {
		return "egress_reference_nanos_per_gib must be non-negative"
	}
	if s.EgressNanosPerGiB < 0 ||
		(s.EgressReferenceNanosPerGiB > 0 && s.EgressNanosPerGiB == 0) {
		return "egress_nanos_per_gib must be positive when the reference rate is positive"
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

// freezeCurrentCostPolicySnapshot is an admission-only loader. Historical
// validation and settlement must use validateFrozenCostPolicySnapshot on the
// snapshot already stored in the PricingDecision instead.
func freezeCurrentCostPolicySnapshot(
	catalogue CataloguePriceAuthority,
	currency string,
) (*FrozenCostPolicySnapshot, error) {
	if parsed, err := ParseCurrency(currency); err != nil || parsed.Code() != currency {
		return nil, errors.New("cost policy requires a supported canonical settlement currency")
	}
	if catalogue.SettlementCurrency != currency {
		return nil, errors.New("cost policy catalogue and decision settlement currencies disagree")
	}
	fx, err := costFXAuthorityFromCatalogue(catalogue)
	if err != nil {
		return nil, fmt.Errorf("freeze catalogue cost FX: %w", err)
	}
	schedule, err := LoadCostScheduleFromEnv(fx)
	if err != nil {
		return nil, fmt.Errorf("load current cost schedule: %w", err)
	}
	if schedule.SettlementCurrency != currency {
		return nil, fmt.Errorf(
			"current cost schedule currency %q does not match decision currency %q",
			schedule.SettlementCurrency, currency,
		)
	}
	digest, err := costScheduleDigest(schedule)
	if err != nil {
		return nil, fmt.Errorf("cost schedule digest: %w", err)
	}
	retention := currentJobObjectRetentionPolicy()
	if err := validateCurrentRetentionPolicyForFrozenCostVersion(
		frozenCostPolicySnapshotVersion, retention,
	); err != nil {
		return nil, err
	}
	snapshot := &FrozenCostPolicySnapshot{
		Version:                 frozenCostPolicySnapshotVersion,
		Schedule:                schedule,
		ScheduleSHA256:          digest,
		FX:                      fx,
		RetentionSeconds:        int64(retention.Duration / time.Second),
		RetentionPolicyRevision: retention.Revision,
		RetentionBasis:          retention.Basis,
	}
	if err := validateFrozenCostPolicySnapshot(snapshot, currency); err != nil {
		return nil, err
	}
	if err := validateCostFXCatalogueBinding(snapshot.FX, catalogue); err != nil {
		return nil, err
	}
	return snapshot, nil
}

func frozenCostPolicyRetentionRevision(version int) (string, bool) {
	switch version {
	case frozenCostPolicySnapshotVersionV1:
		return frozenCostPolicyV1RetentionPolicyRevision, true
	default:
		return "", false
	}
}

// validateCurrentRetentionPolicyForFrozenCostVersion is admission-only. It
// prevents a developer from advancing the live retention revision while still
// emitting v1 snapshots whose historical validator must retain v1 semantics.
func validateCurrentRetentionPolicyForFrozenCostVersion(
	version int,
	retention JobObjectRetentionPolicy,
) error {
	want, ok := frozenCostPolicyRetentionRevision(version)
	if !ok {
		return fmt.Errorf("unsupported current cost policy snapshot version %d", version)
	}
	if retention.Revision != want {
		return fmt.Errorf(
			"current retention policy revision %q is incompatible with frozen cost policy v%d revision %q; bump the frozen snapshot version before admission",
			retention.Revision, version, want,
		)
	}
	return nil
}

func validateFrozenCostPolicySnapshot(snapshot *FrozenCostPolicySnapshot, currency string) error {
	if snapshot == nil {
		return errors.New("cost policy snapshot is required")
	}
	retentionRevision, ok := frozenCostPolicyRetentionRevision(snapshot.Version)
	if !ok {
		return fmt.Errorf("unsupported cost policy snapshot version %d", snapshot.Version)
	}
	if reason := validateCostSchedule(snapshot.Schedule); reason != "" {
		return fmt.Errorf("invalid frozen cost schedule: %s", reason)
	}
	settlement, err := ParseCurrency(currency)
	if err != nil || settlement.Code() != currency {
		return errors.New("frozen cost policy settlement currency is unsupported")
	}
	if err := validateCostFXAuthority(snapshot.FX, settlement); err != nil {
		return fmt.Errorf("invalid frozen cost FX authority: %w", err)
	}
	if snapshot.Schedule.SettlementCurrency != currency {
		return fmt.Errorf(
			"frozen cost schedule currency %q does not match decision currency %q",
			snapshot.Schedule.SettlementCurrency, currency,
		)
	}
	if snapshot.Schedule.ReferenceCurrency != snapshot.FX.ReferenceCurrency ||
		snapshot.Schedule.SettlementCurrency != snapshot.FX.SettlementCurrency {
		return errors.New("frozen cost schedule and FX authority currency pairs disagree")
	}
	if err := validateCostScheduleFXBinding(snapshot.Schedule, snapshot.FX); err != nil {
		return err
	}
	digest, err := costScheduleDigest(snapshot.Schedule)
	if err != nil {
		return err
	}
	if !validSHA256(snapshot.ScheduleSHA256) || digest != snapshot.ScheduleSHA256 {
		return errors.New("frozen cost schedule digest does not match its canonical body")
	}
	if snapshot.RetentionPolicyRevision != retentionRevision {
		return fmt.Errorf(
			"unsupported retention policy revision %q",
			snapshot.RetentionPolicyRevision,
		)
	}
	minimumSeconds := int64((8 * 24 * time.Hour) / time.Second)
	maximumSeconds := int64(^uint64(0)>>1) / int64(time.Second)
	if snapshot.RetentionSeconds < minimumSeconds ||
		snapshot.RetentionSeconds > maximumSeconds ||
		snapshot.RetentionSeconds%int64((24*time.Hour)/time.Second) != 0 {
		return errors.New("frozen retention must be a representable whole-day period of at least 8 days")
	}
	if strings.TrimSpace(snapshot.RetentionBasis) == "" {
		return errors.New("frozen retention basis is required")
	}
	return nil
}

func frozenCostPolicyRetention(snapshot *FrozenCostPolicySnapshot, currency string) (time.Duration, error) {
	if err := validateFrozenCostPolicySnapshot(snapshot, currency); err != nil {
		return 0, err
	}
	return time.Duration(snapshot.RetentionSeconds) * time.Second, nil
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
