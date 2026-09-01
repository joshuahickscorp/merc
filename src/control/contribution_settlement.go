package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ContributionSettlement is the only authority allowed to call a batch-job
// contribution "true net". PricingDecision is immutable acceptance-time
// forecast authority; it never becomes settlement merely because execution
// later completed.
const contributionSettlementVersion = 1

const (
	ContributionStageAcceptedForecast      = "ACCEPTED_FORECAST"
	ContributionStageProvisionalSettlement = "PROVISIONAL_SETTLEMENT"
	ContributionStageFinalSettlement       = "FINAL_SETTLEMENT"
)

const (
	contributionComponentAcceptedModel = "ACCEPTED_MODEL"
	contributionComponentSettled       = "SETTLED"
	contributionComponentNotApplicable = "NOT_APPLICABLE"
	contributionComponentUnknown       = "UNKNOWN"
)

// ContributionSettlementKey prevents a result for one subject, accepted price,
// or currency from being reused for another. PricingDecisionSHA256 is the digest
// stored with the job/quote, not a digest reconstructed from current policy.
type ContributionSettlementKey struct {
	SubjectKind           string `json:"subject_kind"`
	SubjectID             string `json:"subject_id"`
	PricingDecisionSHA256 string `json:"pricing_decision_sha256"`
	Currency              string `json:"currency"`
}

// ContributionSettlementComponent names both an exact amount and its source.
// AmountNanos is absent for UNKNOWN, rather than using an unexplained zero.
type ContributionSettlementComponent struct {
	Status      string `json:"status"`
	AmountNanos *int64 `json:"amount_nanos,omitempty"`
	Source      string `json:"source"`
	Basis       string `json:"basis"`
}

// ContributionSettlement reduces accepted and observed facts in exact
// nano-major-units. TrueNetNanos is structurally absent outside FINAL_SETTLEMENT.
// SettlementSHA256 seals the complete result with that field cleared.
type ContributionSettlement struct {
	Version int                       `json:"version"`
	Key     ContributionSettlementKey `json:"key"`
	Stage   string                    `json:"stage"`

	AcceptedKnownCostContributionNanos int64 `json:"accepted_known_cost_contribution_nanos"`

	BuyerGrossCharge        ContributionSettlementComponent `json:"buyer_gross_charge"`
	BuyerRefunds            ContributionSettlementComponent `json:"buyer_refunds"`
	BuyerNetAmount          ContributionSettlementComponent `json:"buyer_net_amount"`
	SupplierEntitlements    ContributionSettlementComponent `json:"supplier_entitlements"`
	ProcessorFee            ContributionSettlementComponent `json:"processor_fee"`
	ControlPlaneCost        ContributionSettlementComponent `json:"control_plane_cost"`
	StorageCost             ContributionSettlementComponent `json:"storage_cost"`
	EgressCost              ContributionSettlementComponent `json:"egress_cost"`
	ProviderCost            ContributionSettlementComponent `json:"provider_cost"`
	RiskReserve             ContributionSettlementComponent `json:"risk_reserve"`
	PlatformSubsidy         ContributionSettlementComponent `json:"platform_subsidy"`
	ObservedOutputRebate    ContributionSettlementComponent `json:"observed_output_rebate"`
	DisputeSupplierClawback ContributionSettlementComponent `json:"dispute_supplier_clawback"`

	TrueNetNanos *int64   `json:"true_net_nanos,omitempty"`
	Blockers     []string `json:"blockers,omitempty"`

	SettlementSHA256 string `json:"settlement_sha256"`
}

func settlementComponent(status string, nanos int64, source, basis string) ContributionSettlementComponent {
	amount := nanos
	return ContributionSettlementComponent{
		Status: status, AmountNanos: &amount, Source: source, Basis: basis,
	}
}

func unknownSettlementComponent(source, basis string) ContributionSettlementComponent {
	return ContributionSettlementComponent{
		Status: contributionComponentUnknown, Source: source, Basis: basis,
	}
}

type acceptedContributionCostNanos struct {
	Payment  int64
	Control  int64
	Storage  int64
	Egress   int64
	Provider int64
	Risk     int64
}

func exactMicroAlignedPricingComponentNanos(
	name string,
	component PricingCostComponent,
) (int64, error) {
	switch component.Status {
	case pricingCostUnknown, pricingCostNotApplicable:
		if component.Amount != 0 {
			return 0, fmt.Errorf("%s non-modeled component carries money", name)
		}
		return 0, nil
	case pricingCostModeled:
		if component.Amount < 0 || !moneyUSDInDomain(component.Amount) {
			return 0, fmt.Errorf("%s modeled component is outside the money domain", name)
		}
		micros := usdToMicros(component.Amount)
		projected := microsToUSD(micros)
		if component.Amount != projected {
			return 0, fmt.Errorf(
				"%s amount %.12f is not exactly micro-aligned (projection %.12f)",
				name, component.Amount, projected)
		}
		if micros > math.MaxInt64/NanosPerMicro {
			return 0, errMoneyOverflow
		}
		return micros * NanosPerMicro, nil
	default:
		return 0, fmt.Errorf("%s component has invalid status %q", name, component.Status)
	}
}

func exactFrozenPricingComponentNanos(
	name string,
	component PricingCostComponent,
	nanos int64,
) (int64, error) {
	if nanos < 0 {
		return 0, fmt.Errorf("%s exact accepted nanos are negative", name)
	}
	switch component.Status {
	case pricingCostUnknown, pricingCostNotApplicable:
		if nanos != 0 || component.Amount != 0 {
			return 0, fmt.Errorf("%s non-modeled component carries exact money", name)
		}
		return 0, nil
	case pricingCostModeled:
		if component.Amount < 0 || !moneyUSDInDomain(component.Amount) ||
			component.Amount != nanosToEconomicUSD(nanos) {
			return 0, fmt.Errorf(
				"%s float projection %.12f does not exactly match %d accepted nanos",
				name, component.Amount, nanos)
		}
		return nanos, nil
	default:
		return 0, fmt.Errorf("%s component has invalid status %q", name, component.Status)
	}
}

// proveAcceptedContributionCostNanos decomposes the aggregate fixed-point
// variable-cost authority without independently rounding arbitrary floats.
// Payment/control/provider must already be exact micro projections; current
// storage/egress/risk use their frozen nano fields. The decomposition must
// conserve KnownVariableCostsNanos exactly or it cannot feed settlement.
func proveAcceptedContributionCostNanos(
	pricing PricingDecision,
) (acceptedContributionCostNanos, error) {
	var out acceptedContributionCostNanos
	if pricing.FixedPoint == nil {
		return out, errors.New("pricing decision has no fixed-point exact component authority")
	}
	var err error
	if out.Payment, err = exactMicroAlignedPricingComponentNanos(
		"payment", pricing.PaymentCost); err != nil {
		return out, err
	}
	if out.Control, err = exactMicroAlignedPricingComponentNanos(
		"control-plane", pricing.ControlPlaneCost); err != nil {
		return out, err
	}
	if out.Provider, err = exactMicroAlignedPricingComponentNanos(
		"provider", pricing.ProviderCost); err != nil {
		return out, err
	}
	if pricing.CostPolicy != nil {
		if out.Storage, err = exactFrozenPricingComponentNanos(
			"storage", pricing.StorageCost, pricing.StorageAcceptedNanos); err != nil {
			return out, err
		}
		if out.Egress, err = exactFrozenPricingComponentNanos(
			"egress", pricing.EgressCost, pricing.EgressAcceptedNanos); err != nil {
			return out, err
		}
		if out.Risk, err = exactFrozenPricingComponentNanos(
			"risk reserve", pricing.RiskReserve, pricing.RiskReserveAcceptedNanos); err != nil {
			return out, err
		}
	} else {
		if pricing.StorageAcceptedNanos != 0 || pricing.EgressAcceptedNanos != 0 ||
			pricing.RiskReserveAcceptedNanos != 0 {
			return out, errors.New("pricing carries exact accepted costs without a frozen cost policy")
		}
		if out.Storage, err = exactMicroAlignedPricingComponentNanos(
			"storage", pricing.StorageCost); err != nil {
			return out, err
		}
		if out.Egress, err = exactMicroAlignedPricingComponentNanos(
			"egress", pricing.EgressCost); err != nil {
			return out, err
		}
		if out.Risk, err = exactMicroAlignedPricingComponentNanos(
			"risk reserve", pricing.RiskReserve); err != nil {
			return out, err
		}
	}
	var total int64
	for _, amount := range []int64{
		out.Payment, out.Control, out.Storage, out.Egress, out.Provider, out.Risk,
	} {
		if err := checkedContributionAdd(&total, amount); err != nil {
			return out, err
		}
	}
	if total != pricing.FixedPoint.KnownVariableCostsNanos {
		return out, fmt.Errorf(
			"exact accepted component sum %d does not conserve fixed-point known variable costs %d",
			total, pricing.FixedPoint.KnownVariableCostsNanos)
	}
	return out, nil
}

func acceptedPricingComponent(
	component PricingCostComponent,
	exactNanos int64,
	exactProven bool,
	source string,
) ContributionSettlementComponent {
	switch component.Status {
	case pricingCostModeled:
		if !exactProven {
			return unknownSettlementComponent(
				source, "accepted model has no exactly conserved component-nano authority")
		}
		return settlementComponent(contributionComponentAcceptedModel, exactNanos, source, component.Basis)
	case pricingCostNotApplicable:
		return settlementComponent(contributionComponentNotApplicable, 0, source, component.Basis)
	default:
		return unknownSettlementComponent(source, component.Basis)
	}
}

func appendContributionBlocker(blockers []string, blocker string) []string {
	blocker = strings.TrimSpace(blocker)
	if blocker == "" {
		return blockers
	}
	for _, existing := range blockers {
		if existing == blocker {
			return blockers
		}
	}
	return append(blockers, blocker)
}

func sealContributionSettlement(out *ContributionSettlement) error {
	if out == nil {
		return errors.New("nil contribution settlement")
	}
	out.SettlementSHA256 = ""
	digest, err := canonicalDigest("contribution settlement", *out)
	if err != nil {
		return err
	}
	out.SettlementSHA256 = digest
	return nil
}

func validateContributionSettlement(out ContributionSettlement) error {
	if out.Version != contributionSettlementVersion ||
		(out.Stage != ContributionStageAcceptedForecast &&
			out.Stage != ContributionStageProvisionalSettlement &&
			out.Stage != ContributionStageFinalSettlement) {
		return errors.New("unsupported contribution settlement version or stage")
	}
	if strings.TrimSpace(out.Key.SubjectKind) == "" || strings.TrimSpace(out.Key.SubjectID) == "" ||
		!validSHA256(out.Key.PricingDecisionSHA256) {
		return errors.New("contribution settlement lacks its subject or pricing digest key")
	}
	currency, err := ParseCurrency(out.Key.Currency)
	if err != nil || currency.Code() != out.Key.Currency {
		return errors.New("contribution settlement has unsupported currency")
	}
	if out.Stage == ContributionStageFinalSettlement {
		if out.TrueNetNanos == nil || len(out.Blockers) != 0 {
			return errors.New("final contribution settlement lacks true net or carries blockers")
		}
	} else {
		if out.TrueNetNanos != nil {
			return errors.New("non-final contribution settlement claims true net")
		}
		if len(out.Blockers) == 0 {
			return errors.New("non-final contribution settlement lacks a finality blocker")
		}
	}
	if out.AcceptedKnownCostContributionNanos < 0 {
		return errors.New("accepted known-cost contribution is negative")
	}
	components := []struct {
		name      string
		component ContributionSettlementComponent
	}{
		{"buyer_gross_charge", out.BuyerGrossCharge},
		{"buyer_refunds", out.BuyerRefunds},
		{"buyer_net_amount", out.BuyerNetAmount},
		{"supplier_entitlements", out.SupplierEntitlements},
		{"processor_fee", out.ProcessorFee},
		{"control_plane_cost", out.ControlPlaneCost},
		{"storage_cost", out.StorageCost},
		{"egress_cost", out.EgressCost},
		{"provider_cost", out.ProviderCost},
		{"risk_reserve", out.RiskReserve},
		{"platform_subsidy", out.PlatformSubsidy},
		{"observed_output_rebate", out.ObservedOutputRebate},
		{"dispute_supplier_clawback", out.DisputeSupplierClawback},
	}
	for _, item := range components {
		component := item.component
		if strings.TrimSpace(component.Source) == "" || strings.TrimSpace(component.Basis) == "" {
			return fmt.Errorf("contribution component %s lacks an exact source or basis", item.name)
		}
		switch component.Status {
		case contributionComponentUnknown:
			if component.AmountNanos != nil {
				return fmt.Errorf("unknown contribution component %s carries an amount", item.name)
			}
		case contributionComponentAcceptedModel, contributionComponentSettled,
			contributionComponentNotApplicable:
			if component.AmountNanos == nil || *component.AmountNanos < 0 {
				return fmt.Errorf("known contribution component %s lacks a nonnegative exact amount", item.name)
			}
			if component.Status == contributionComponentNotApplicable && *component.AmountNanos != 0 {
				return fmt.Errorf("not-applicable contribution component %s carries money", item.name)
			}
		default:
			return fmt.Errorf("contribution component %s has invalid status %q", item.name, component.Status)
		}
	}
	want := out.SettlementSHA256
	if !validSHA256(want) {
		return errors.New("contribution settlement lacks a valid digest")
	}
	out.SettlementSHA256 = ""
	got, err := canonicalDigest("contribution settlement", out)
	if err != nil {
		return err
	}
	if got != want {
		return errors.New("contribution settlement digest mismatch")
	}
	return nil
}

// acceptedForecastContributionSettlement exposes the accepted known-cost
// contribution and nothing stronger. In particular, even an older decision
// carrying FixedPoint.TrueNetContributionNanos remains an ACCEPTED_FORECAST.
func acceptedForecastContributionSettlement(
	subjectKind, subjectID string,
	pricing PricingDecision,
) (ContributionSettlement, error) {
	pricingSHA, err := pricingDecisionDigest(pricing)
	if err != nil {
		return ContributionSettlement{}, err
	}
	costNanos, costProofErr := proveAcceptedContributionCostNanos(pricing)
	exactCostsProven := costProofErr == nil
	supplierNanos := int64(0)
	supplierExact := false
	if pricing.FixedPoint != nil {
		supplierNanos = pricing.FixedPoint.SupplierEntitlementsNanos
		supplierExact = true
	}
	out := ContributionSettlement{
		Version: contributionSettlementVersion,
		Key: ContributionSettlementKey{
			SubjectKind: subjectKind, SubjectID: subjectID,
			PricingDecisionSHA256: pricingSHA, Currency: pricing.Currency,
		},
		Stage: ContributionStageAcceptedForecast,
		BuyerGrossCharge: unknownSettlementComponent(
			"settlement ledger", "buyer settlement has not occurred"),
		BuyerRefunds: settlementComponent(
			contributionComponentAcceptedModel, 0, "accepted forecast",
			"refunds are outcome facts and are not forecast as settled money"),
		BuyerNetAmount: unknownSettlementComponent(
			"settlement ledger", "buyer settlement has not occurred"),
		SupplierEntitlements: acceptedPricingComponent(
			pricing.PrimarySupplierCost, supplierNanos, supplierExact,
			"pricing_decision.fixed_point.supplier_entitlements_nanos"),
		ProcessorFee: acceptedPricingComponent(
			pricing.PaymentCost, costNanos.Payment, exactCostsProven,
			"pricing_decision.payment_cost"),
		ControlPlaneCost: acceptedPricingComponent(
			pricing.ControlPlaneCost, costNanos.Control, exactCostsProven,
			"pricing_decision.control_plane_cost"),
		StorageCost: acceptedPricingComponent(
			pricing.StorageCost, costNanos.Storage, exactCostsProven,
			"pricing_decision.cost_policy.storage_accepted_nanos"),
		EgressCost: acceptedPricingComponent(
			pricing.EgressCost, costNanos.Egress, exactCostsProven,
			"pricing_decision.cost_policy.egress_accepted_nanos"),
		ProviderCost: acceptedPricingComponent(
			pricing.ProviderCost, costNanos.Provider, exactCostsProven,
			"pricing_decision.provider_cost"),
		RiskReserve: acceptedPricingComponent(
			pricing.RiskReserve, costNanos.Risk, exactCostsProven,
			"pricing_decision.cost_policy.risk_reserve_accepted_nanos"),
		PlatformSubsidy: unknownSettlementComponent(
			"supplier_payout_funding", "subsidy is an observed funding fact"),
		ObservedOutputRebate: unknownSettlementComponent(
			"ledger_entries.buyer_charge", "observed-output rebate is known only at settlement"),
		DisputeSupplierClawback: unknownSettlementComponent(
			"ledger_entries.clawback", "dispute and clawback are outcome facts"),
		Blockers: []string{"settlement has not started"},
	}
	if costProofErr != nil {
		out.Blockers = appendContributionBlocker(out.Blockers,
			"accepted exact component authority: "+costProofErr.Error())
	}
	if pricing.FixedPoint == nil {
		out.Blockers = appendContributionBlocker(out.Blockers, "pricing decision has no exact fixed-point authority")
	} else {
		if pricing.FixedPoint.Currency != pricing.Currency {
			return ContributionSettlement{}, errors.New("pricing fixed-point currency disagrees with decision")
		}
		out.AcceptedKnownCostContributionNanos = pricing.FixedPoint.KnownCostContributionNanos
		out.SupplierEntitlements = settlementComponent(
			contributionComponentAcceptedModel,
			pricing.FixedPoint.SupplierEntitlementsNanos,
			"pricing_decision.fixed_point.supplier_entitlements_nanos",
			"accepted supplier entitlement forecast",
		)
		for _, category := range pricing.FixedPoint.UnknownCostCategories {
			out.Blockers = appendContributionBlocker(out.Blockers, "accepted cost unknown: "+category)
		}
	}
	if pricing.ExecutionMode == computeExecutionDistributed {
		if pricing.RuntimeCell == nil {
			out.Blockers = appendContributionBlocker(out.Blockers, "accepted runtime-cell economics unavailable")
		} else if !pricing.RuntimeCell.MercTrueNetAvailable() {
			for _, category := range pricing.RuntimeCell.UnknownCategories {
				out.Blockers = appendContributionBlocker(out.Blockers, "accepted runtime-cell cost unknown: "+category)
			}
			if len(pricing.RuntimeCell.UnknownCategories) == 0 {
				out.Blockers = appendContributionBlocker(out.Blockers,
					"accepted runtime-cell economics refuses true net: "+pricing.RuntimeCell.MercTrueNetStatus)
			}
		}
	}
	if err := sealContributionSettlement(&out); err != nil {
		return ContributionSettlement{}, err
	}
	return out, validateContributionSettlement(out)
}

type contributionJobFacts struct {
	Status                    string
	Pricing                   PricingDecision
	PricingSHA256             string
	Currency                  string
	SLAGuaranteeSecs          int
	SLAMet                    *bool
	ChargeBatchID             *uuid.UUID
	StripePI                  *string
	HasSettlementLedger       bool
	BuyerGrossNanos           int64
	BuyerRefundNanos          int64
	SLARefundNanos            int64
	SupplierCreditNanos       int64
	SupplierClawbackNanos     int64
	RiskAccrualNanos          int64
	RiskHeldNanos             int64
	RiskReleaseNanos          int64
	RiskConsumeNanos          int64
	RiskCanonical             bool
	ProcessorFeeNanos         *int64
	ProcessorFeeSource        string
	SubsidyNanos              int64
	ObservedOutputRebateNanos int64
	OpenDisputes              int64
	DisputeRefundNanos        int64
}

func loadContributionObservedOutputRebate(
	ctx context.Context,
	tx pgx.Tx,
	jobID uuid.UUID,
) (int64, error) {
	return loadContributionObservedOutputRebateForJob(ctx, tx, jobID)
}

func checkedContributionAdd(total *int64, value int64) error {
	if total == nil {
		return errors.New("nil contribution total")
	}
	next := *total + value
	if (value > 0 && next < *total) || (value < 0 && next > *total) {
		return errMoneyOverflow
	}
	*total = next
	return nil
}

func requireContributionCurrency(jobID uuid.UUID, want, got string) error {
	if got == "" || got != want {
		return fmt.Errorf("%w: job %s contribution facts mix %q and %q",
			errCurrencyMismatch, jobID, want, got)
	}
	return nil
}

func loadContributionProcessorFee(
	ctx context.Context,
	tx pgx.Tx,
	jobID uuid.UUID,
	chargeBatchID *uuid.UUID,
	stripePI *string,
	currency string,
) (*int64, string, error) {
	if chargeBatchID == nil {
		if stripePI == nil || strings.TrimSpace(*stripePI) == "" {
			return nil, "", nil
		}
		var feeNanos int64
		var feeCurrency string
		err := tx.QueryRow(ctx, `
			SELECT (-amount_usd*1000000000)::bigint,currency
			  FROM ledger_entries
			 WHERE kind='stripe_fee' AND payout_ref=$1`, *stripePI).
			Scan(&feeNanos, &feeCurrency)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, "", nil
		}
		if err != nil {
			return nil, "", err
		}
		if feeNanos < 0 {
			return nil, "", fmt.Errorf("job %s processor fee has an invalid sign", jobID)
		}
		if err := requireContributionCurrency(jobID, currency, feeCurrency); err != nil {
			return nil, "", err
		}
		return &feeNanos, "ledger_entries.stripe_fee:payout_ref", nil
	}

	var memberCount, allocationCount, allocationMethodCount int64
	var feeNanos, allocatedNanos int64
	var jobFeeNanos *int64
	var allocationMethod, feeCurrency *string
	err := tx.QueryRow(ctx, `SELECT
			(SELECT COUNT(*) FROM jobs j WHERE j.charge_batch_id=cb.id),
			(SELECT COUNT(*) FROM charge_batch_fee_allocations a
			 WHERE a.charge_batch_id=cb.id),
			(SELECT COUNT(DISTINCT a.allocation_method)
			 FROM charge_batch_fee_allocations a WHERE a.charge_batch_id=cb.id),
			(-fee.amount_usd*1000000000)::bigint,
			(SELECT (COALESCE(SUM(a.allocated_fee_usd),0)*1000000000)::bigint
			 FROM charge_batch_fee_allocations a WHERE a.charge_batch_id=cb.id),
			(SELECT (a.allocated_fee_usd*1000000000)::bigint
			 FROM charge_batch_fee_allocations a
			 WHERE a.charge_batch_id=cb.id AND a.job_id=$2),
			(SELECT a.allocation_method
			 FROM charge_batch_fee_allocations a
			 WHERE a.charge_batch_id=cb.id AND a.job_id=$2),
			fee.currency
		FROM charge_batches cb
		JOIN ledger_entries fee
		  ON fee.kind='stripe_fee' AND fee.payout_ref=cb.stripe_pi
		WHERE cb.id=$1 AND cb.status='charged'`, *chargeBatchID, jobID).
		Scan(&memberCount, &allocationCount, &allocationMethodCount,
			&feeNanos, &allocatedNanos, &jobFeeNanos, &allocationMethod, &feeCurrency)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, "", nil
	}
	if err != nil {
		return nil, "", err
	}
	if feeCurrency == nil || requireContributionCurrency(jobID, currency, *feeCurrency) != nil {
		return nil, "", fmt.Errorf("%w: job %s processor allocation currency is not %s",
			errCurrencyMismatch, jobID, currency)
	}
	if memberCount <= 0 || allocationCount != memberCount || allocationMethodCount != 1 ||
		allocationMethod == nil ||
		(*allocationMethod != batchFeeAllocationHamiltonV1 && *allocationMethod != batchFeeAllocationLegacyV0) ||
		feeNanos < 0 || allocatedNanos != feeNanos || jobFeeNanos == nil || *jobFeeNanos < 0 {
		return nil, "", fmt.Errorf(
			"job %s charge batch processor-fee allocation is incomplete or inconsistent", jobID)
	}
	return jobFeeNanos, "charge_batch_fee_allocations:" + *allocationMethod, nil
}

func loadContributionJobFacts(ctx context.Context, tx pgx.Tx, jobID uuid.UUID) (contributionJobFacts, error) {
	var facts contributionJobFacts
	var pricingBlob []byte
	err := tx.QueryRow(ctx, `
		SELECT status,currency,pricing_decision,
		       COALESCE(pricing_decision_sha256,''),charge_batch_id,stripe_pi,
		       COALESCE(sla_guarantee_secs,0),sla_met
		  FROM jobs WHERE id=$1`, jobID).
		Scan(&facts.Status, &facts.Currency, &pricingBlob, &facts.PricingSHA256,
			&facts.ChargeBatchID, &facts.StripePI, &facts.SLAGuaranteeSecs, &facts.SLAMet)
	if errors.Is(err, pgx.ErrNoRows) {
		return facts, errNotFound
	}
	if err != nil {
		return facts, err
	}
	if len(pricingBlob) == 0 {
		return facts, errors.New("job has no pricing decision settlement authority")
	}
	if err := json.Unmarshal(pricingBlob, &facts.Pricing); err != nil {
		return facts, fmt.Errorf("decode pricing decision for contribution: %w", err)
	}
	pricingSHA, err := pricingDecisionDigest(facts.Pricing)
	if err != nil {
		return facts, err
	}
	if !validSHA256(facts.PricingSHA256) || facts.PricingSHA256 != pricingSHA {
		return facts, fmt.Errorf("job %s pricing decision digest mismatch", jobID)
	}
	if facts.Pricing.Currency != facts.Currency {
		return facts, fmt.Errorf("%w: job %s pricing currency %s differs from job currency %s",
			errCurrencyMismatch, jobID, facts.Pricing.Currency, facts.Currency)
	}
	if _, err := ParseCurrency(facts.Currency); err != nil {
		return facts, err
	}

	rows, err := tx.Query(ctx, `
		SELECT le.kind,(le.amount_usd*1000000000)::bigint,le.currency
		  FROM ledger_entries le
		 WHERE le.task_id IN (SELECT id FROM tasks WHERE job_id=$1)
		    OR le.payout_ref IN ($2,$3,$4,$5,$6)
		    OR le.payout_ref IN (
		         SELECT 'dispute-sla-refund-' || dispute_id::text
		           FROM job_dispute_refunds WHERE job_id=$1
		       )
		 ORDER BY le.created_at,le.id`,
		jobID, slaPremiumChargeRef(jobID), slaRefundRef(jobID),
		riskReserveAccrualRef(jobID), riskReserveReleaseRef(jobID), riskReserveConsumeRef(jobID))
	if err != nil {
		return facts, err
	}
	defer rows.Close()
	for rows.Next() {
		var kind, currency string
		var amount int64
		if err := rows.Scan(&kind, &amount, &currency); err != nil {
			return facts, err
		}
		if err := requireContributionCurrency(jobID, facts.Currency, currency); err != nil {
			return facts, err
		}
		facts.HasSettlementLedger = true
		switch kind {
		case KindBuyerCharge:
			if amount > 0 {
				return facts, errors.New("buyer charge has an invalid positive sign")
			}
			err = checkedContributionAdd(&facts.BuyerGrossNanos, -amount)
		case KindBuyerRefund:
			if amount < 0 {
				return facts, errors.New("buyer refund has an invalid negative sign")
			}
			err = checkedContributionAdd(&facts.BuyerRefundNanos, amount)
		case KindSLARefund:
			if amount < 0 {
				return facts, errors.New("SLA refund has an invalid negative sign")
			}
			err = checkedContributionAdd(&facts.SLARefundNanos, amount)
		case KindSupplierCredit:
			if amount < 0 {
				return facts, errors.New("supplier credit has an invalid negative sign")
			}
			err = checkedContributionAdd(&facts.SupplierCreditNanos, amount)
		case KindClawback:
			if amount > 0 {
				return facts, errors.New("supplier clawback has an invalid positive sign")
			}
			err = checkedContributionAdd(&facts.SupplierClawbackNanos, -amount)
		case KindRiskReserveAccrual:
			err = checkedContributionAdd(&facts.RiskAccrualNanos, amount)
		case KindRiskReserveRelease:
			err = checkedContributionAdd(&facts.RiskReleaseNanos, -amount)
		case KindRiskReserveConsume:
			err = checkedContributionAdd(&facts.RiskConsumeNanos, -amount)
		}
		if err != nil {
			return facts, err
		}
	}
	if err := rows.Err(); err != nil {
		return facts, err
	}
	if facts.BuyerRefundNanos > math.MaxInt64-facts.SLARefundNanos {
		return facts, errMoneyOverflow
	}
	// The exact state row, not the micro-ledger projection, is current reserve
	// authority. It is keyed by the same job/pricing digest/currency triple as
	// ContributionSettlement.
	var riskPricingSHA, riskCurrency string
	err = tx.QueryRow(ctx, `
		SELECT pricing_decision_sha256,currency,
		       accrued_nanos,held_nanos,consumed_nanos,released_nanos
		  FROM job_risk_reserves WHERE job_id=$1`, jobID).
		Scan(&riskPricingSHA, &riskCurrency,
			&facts.RiskAccrualNanos, &facts.RiskHeldNanos,
			&facts.RiskConsumeNanos, &facts.RiskReleaseNanos)
	if err == nil {
		facts.RiskCanonical = true
		if riskPricingSHA != facts.PricingSHA256 {
			return facts, errors.New("risk reserve pricing digest disagrees with contribution key")
		}
		if err := requireContributionCurrency(jobID, facts.Currency, riskCurrency); err != nil {
			return facts, err
		}
		closedRiskNanos := facts.RiskHeldNanos
		if err := checkedContributionAdd(&closedRiskNanos, facts.RiskConsumeNanos); err != nil {
			return facts, err
		}
		if err := checkedContributionAdd(&closedRiskNanos, facts.RiskReleaseNanos); err != nil {
			return facts, err
		}
		if facts.RiskAccrualNanos <= 0 || facts.RiskHeldNanos < 0 ||
			facts.RiskConsumeNanos < 0 || facts.RiskReleaseNanos < 0 ||
			facts.RiskAccrualNanos != closedRiskNanos {
			return facts, errors.New("risk reserve exact-nano state does not conserve")
		}
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return facts, err
	}

	facts.ProcessorFeeNanos, facts.ProcessorFeeSource, err = loadContributionProcessorFee(
		ctx, tx, jobID, facts.ChargeBatchID, facts.StripePI, facts.Currency)
	if err != nil {
		return facts, err
	}
	facts.ObservedOutputRebateNanos, err = loadContributionObservedOutputRebate(
		ctx, tx, jobID)
	if err != nil {
		return facts, err
	}

	var subsidyMinorUnits int64
	var subsidyCurrencies []string
	rows, err = tx.Query(ctx, `
		SELECT currency,COALESCE(SUM(amount_cents),0)::bigint
		  FROM supplier_payout_funding
		 WHERE liability_job_id=$1 AND source_kind='platform_subsidy'
		 GROUP BY currency ORDER BY currency`, jobID)
	if err != nil {
		return facts, err
	}
	for rows.Next() {
		var currency string
		var amount int64
		if err := rows.Scan(&currency, &amount); err != nil {
			rows.Close()
			return facts, err
		}
		subsidyCurrencies = append(subsidyCurrencies, currency)
		if err := requireContributionCurrency(jobID, facts.Currency, currency); err != nil {
			rows.Close()
			return facts, err
		}
		if err := checkedContributionAdd(&subsidyMinorUnits, amount); err != nil {
			rows.Close()
			return facts, err
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return facts, err
	}
	if len(subsidyCurrencies) > 1 {
		return facts, fmt.Errorf("%w: job %s subsidy funding mixes currencies", errCurrencyMismatch, jobID)
	}
	currency, _ := ParseCurrency(facts.Currency)
	subsidyMicros, err := currency.MinorToMicros(subsidyMinorUnits)
	if err != nil {
		return facts, err
	}
	if subsidyMicros > math.MaxInt64/NanosPerMicro {
		return facts, errMoneyOverflow
	}
	facts.SubsidyNanos = subsidyMicros * NanosPerMicro

	if err := tx.QueryRow(ctx, `
		SELECT COUNT(*) FILTER (WHERE status IN ('open','no_peer','reverifying','unresolvable'))
		  FROM disputes WHERE job_id=$1`, jobID).Scan(&facts.OpenDisputes); err != nil {
		return facts, err
	}
	var disputeCurrency *string
	err = tx.QueryRow(ctx, `
		SELECT (COALESCE(SUM(buyer_refund_usd),0)*1000000000)::bigint,
		       MIN(currency)
		  FROM job_dispute_refunds WHERE job_id=$1`, jobID).
		Scan(&facts.DisputeRefundNanos, &disputeCurrency)
	if err != nil {
		return facts, err
	}
	if disputeCurrency != nil {
		if err := requireContributionCurrency(jobID, facts.Currency, *disputeCurrency); err != nil {
			return facts, err
		}
	}
	return facts, nil
}

func contributionStatusTerminal(status string) bool {
	switch status {
	case "complete", "failed", "cancelled":
		return true
	default:
		return false
	}
}

func reduceContributionJobFacts(jobID uuid.UUID, facts contributionJobFacts) (ContributionSettlement, error) {
	forecast, err := acceptedForecastContributionSettlement("job", jobID.String(), facts.Pricing)
	if err != nil {
		return ContributionSettlement{}, err
	}
	if forecast.Key.PricingDecisionSHA256 != facts.PricingSHA256 ||
		forecast.Key.Currency != facts.Currency {
		return ContributionSettlement{}, errors.New("contribution key disagrees with durable job authority")
	}
	out := forecast
	out.SettlementSHA256 = ""
	// Settlement facts supersede the generic forecast blocker, while every
	// accepted unknown remains a real blocker.
	out.Blockers = nil
	for _, blocker := range forecast.Blockers {
		if blocker != "settlement has not started" {
			out.Blockers = appendContributionBlocker(out.Blockers, blocker)
		}
	}
	if !facts.HasSettlementLedger {
		out.Blockers = appendContributionBlocker(out.Blockers, "settlement ledger unavailable")
	} else {
		out.Stage = ContributionStageProvisionalSettlement
	}
	if !contributionStatusTerminal(facts.Status) {
		out.Blockers = appendContributionBlocker(out.Blockers, "job is not terminal: "+facts.Status)
	}

	totalRefunds := facts.BuyerRefundNanos + facts.SLARefundNanos
	if totalRefunds > facts.BuyerGrossNanos {
		return ContributionSettlement{}, errors.New("buyer refunds exceed gross charge")
	}
	if facts.BuyerGrossNanos > 0 {
		out.BuyerGrossCharge = settlementComponent(contributionComponentSettled,
			facts.BuyerGrossNanos, "ledger_entries.buyer_charge",
			"gross accepted charges posted for this job")
		out.BuyerRefunds = settlementComponent(contributionComponentSettled,
			totalRefunds, "ledger_entries.buyer_refund+sla_refund",
			"buyer refunds, including the speed-SLA refund exactly once")
		buyerNet := facts.BuyerGrossNanos - totalRefunds
		out.BuyerNetAmount = settlementComponent(contributionComponentSettled,
			buyerNet, "ledger_entries.buyer_charge+buyer_refund+sla_refund",
			"gross buyer charge less dispute and SLA refunds")
	} else {
		out.Blockers = appendContributionBlocker(out.Blockers, "buyer charge settlement unavailable")
	}

	if facts.SupplierClawbackNanos > facts.SupplierCreditNanos {
		return ContributionSettlement{}, errors.New("supplier clawback exceeds supplier credits")
	}
	supplierNet := facts.SupplierCreditNanos - facts.SupplierClawbackNanos
	if facts.SupplierCreditNanos > 0 || facts.Pricing.ExecutionMode == computeExecutionExactReuse {
		out.SupplierEntitlements = settlementComponent(contributionComponentSettled,
			supplierNet, "ledger_entries.supplier_credit+clawback",
			"net supplier entitlement after dispute clawbacks")
		out.DisputeSupplierClawback = settlementComponent(contributionComponentSettled,
			facts.SupplierClawbackNanos, "ledger_entries.clawback",
			"supplier credits reversed by verification or dispute")
	} else {
		out.Blockers = appendContributionBlocker(out.Blockers, "supplier settlement unavailable")
	}

	if facts.ProcessorFeeNanos == nil {
		out.ProcessorFee = unknownSettlementComponent(
			"ledger_entries.stripe_fee/charge_batch_fee_allocations",
			"processor cash fee has not been completely reconciled and allocated")
		out.Blockers = appendContributionBlocker(out.Blockers, "processor fee allocation unavailable")
	} else {
		out.ProcessorFee = settlementComponent(contributionComponentSettled,
			*facts.ProcessorFeeNanos, facts.ProcessorFeeSource,
			"actual processor fee allocated from the collected charge")
	}

	// Control-plane and provider costs do not yet have a separate actual meter.
	// A frozen accepted model closes them only when its component nanos were
	// exactly decomposed and conserved against FixedPoint above.
	out.ControlPlaneCost = forecast.ControlPlaneCost
	if out.ControlPlaneCost.Status == contributionComponentUnknown {
		out.Blockers = appendContributionBlocker(out.Blockers, "control-plane cost unavailable")
	}
	out.ProviderCost = forecast.ProviderCost
	if out.ProviderCost.Status == contributionComponentUnknown {
		out.Blockers = appendContributionBlocker(out.Blockers, "provider cost unavailable")
	}

	// job_cost_settlements is currently caller-supplied and binds no canonical
	// object or transfer provenance. An accepted byte bound is forecast evidence,
	// not an actual. Until source-bound artifact/transfer settlement exists,
	// modeled storage or egress remains an explicit finality blocker. N/A can
	// close at exact zero because no applicable transfer/storage fact exists.
	settleUnboundTransferCost := func(
		name string,
		component PricingCostComponent,
	) ContributionSettlementComponent {
		if component.Status == pricingCostNotApplicable {
			return settlementComponent(contributionComponentNotApplicable, 0,
				"pricing_decision."+name, component.Basis)
		}
		out.Blockers = appendContributionBlocker(out.Blockers,
			name+" lacks source-bound artifact/transfer settlement provenance")
		return unknownSettlementComponent(
			"source-bound artifact/transfer settlement",
			"accepted "+name+" is forecast only; canonical actual provenance is unavailable")
	}
	out.StorageCost = settleUnboundTransferCost("storage_cost", facts.Pricing.StorageCost)
	out.EgressCost = settleUnboundTransferCost("egress_cost", facts.Pricing.EgressCost)

	// Refunds already reduce BuyerNetAmount. Reserve consumption must not be
	// subtracted again for the same loss. Once the lifecycle closes, its final
	// contribution cost is therefore zero; the terminal ledger row is the source.
	switch facts.Pricing.RiskReserve.Status {
	case pricingCostNotApplicable:
		out.RiskReserve = settlementComponent(contributionComponentNotApplicable, 0,
			"pricing_decision.risk_reserve", facts.Pricing.RiskReserve.Basis)
	case pricingCostUnknown:
		out.RiskReserve = unknownSettlementComponent(
			"pricing_decision.risk_reserve", facts.Pricing.RiskReserve.Basis)
		out.Blockers = appendContributionBlocker(out.Blockers, "risk reserve authority unavailable")
	case pricingCostModeled:
		if !facts.RiskCanonical {
			out.RiskReserve = unknownSettlementComponent(
				"job_risk_reserves", "canonical exact-nano risk reserve was not accrued")
			out.Blockers = appendContributionBlocker(out.Blockers,
				"canonical risk reserve accrual unavailable")
		} else if facts.RiskHeldNanos == 0 {
			source := "job_risk_reserves.released_nanos"
			basis := "reserve fully released after the dispute window; no final cost remains"
			if facts.RiskConsumeNanos > 0 && facts.RiskReleaseNanos > 0 {
				source = "job_risk_reserves.consumed_nanos+released_nanos"
				basis = "reserve partially consumed against refunds and the remainder released; refunds are already in buyer net"
			} else if facts.RiskConsumeNanos > 0 {
				source = "job_risk_reserves.consumed_nanos"
				basis = "reserve consumed against refunds already included in buyer net; not subtracted twice"
			}
			out.RiskReserve = settlementComponent(contributionComponentSettled, 0, source, basis)
		} else {
			out.RiskReserve = unknownSettlementComponent(
				"job_risk_reserves.held_nanos",
				"reserve remains open until release or consumption closes the dispute window")
			out.Blockers = appendContributionBlocker(out.Blockers, "risk reserve lifecycle remains open")
		}
	}

	out.PlatformSubsidy = settlementComponent(contributionComponentSettled,
		facts.SubsidyNanos, "supplier_payout_funding.platform_subsidy",
		"authorized platform funding of supplier cash, deducted exactly once")
	out.ObservedOutputRebate = settlementComponent(contributionComponentSettled,
		facts.ObservedOutputRebateNanos,
		"tasks.economic_buyer_charge_nanos+reported_tokens_used",
		"exact accepted task charge less its observed-output settlement; informational and already reflected in buyer net")

	if facts.SLAGuaranteeSecs > 0 && facts.SLAMet == nil {
		out.Blockers = appendContributionBlocker(out.Blockers, "SLA outcome is unresolved")
	}
	if facts.OpenDisputes > 0 {
		out.Blockers = appendContributionBlocker(out.Blockers, "open dispute remains")
	}
	if facts.DisputeRefundNanos > facts.BuyerRefundNanos {
		out.Blockers = appendContributionBlocker(out.Blockers,
			"dispute refund receipt exceeds buyer-refund ledger facts")
	}
	if facts.Pricing.ExecutionMode == computeExecutionDistributed {
		if facts.Pricing.RuntimeCell == nil {
			out.Blockers = appendContributionBlocker(out.Blockers, "accepted runtime-cell economics unavailable")
		} else if !facts.Pricing.RuntimeCell.MercTrueNetAvailable() {
			for _, category := range facts.Pricing.RuntimeCell.UnknownCategories {
				out.Blockers = appendContributionBlocker(out.Blockers,
					"accepted runtime-cell cost unknown: "+category)
			}
			if len(facts.Pricing.RuntimeCell.UnknownCategories) == 0 {
				out.Blockers = appendContributionBlocker(out.Blockers,
					"accepted runtime-cell economics refuses true net: "+facts.Pricing.RuntimeCell.MercTrueNetStatus)
			}
		}
	}
	if facts.Pricing.FixedPoint != nil {
		for _, category := range facts.Pricing.FixedPoint.UnknownCostCategories {
			out.Blockers = appendContributionBlocker(out.Blockers, "accepted cost unknown: "+category)
		}
	}

	if len(out.Blockers) == 0 {
		buyerNet := facts.BuyerGrossNanos - totalRefunds
		processor := *facts.ProcessorFeeNanos
		componentAmount := func(c ContributionSettlementComponent) (int64, error) {
			if c.AmountNanos == nil {
				return 0, errors.New("final contribution component has no exact amount")
			}
			return *c.AmountNanos, nil
		}
		control, err := componentAmount(out.ControlPlaneCost)
		if err != nil {
			return ContributionSettlement{}, err
		}
		storage, err := componentAmount(out.StorageCost)
		if err != nil {
			return ContributionSettlement{}, err
		}
		egress, err := componentAmount(out.EgressCost)
		if err != nil {
			return ContributionSettlement{}, err
		}
		provider, err := componentAmount(out.ProviderCost)
		if err != nil {
			return ContributionSettlement{}, err
		}
		total, err := NewMoneyNanos(MustParseCurrency(facts.Currency), buyerNet)
		if err != nil {
			return ContributionSettlement{}, err
		}
		for _, cost := range []int64{
			supplierNet, processor, control, storage, egress, provider, facts.SubsidyNanos,
		} {
			other, _ := NewMoneyNanos(total.Currency, cost)
			total, err = total.Sub(other)
			if err != nil {
				return ContributionSettlement{}, err
			}
		}
		trueNet := total.Nanos
		out.TrueNetNanos = &trueNet
		out.Stage = ContributionStageFinalSettlement
	} else if out.Stage != ContributionStageAcceptedForecast {
		sort.Strings(out.Blockers)
	}
	if err := sealContributionSettlement(&out); err != nil {
		return ContributionSettlement{}, err
	}
	return out, validateContributionSettlement(out)
}

// ContributionSettlementForJob loads all facts that can move contribution in
// one read-only REPEATABLE READ snapshot. This prevents a receipt from combining
// a pre-refund buyer amount with a post-clawback supplier amount (or the inverse).
func (s *Store) ContributionSettlementForJob(
	ctx context.Context,
	jobID uuid.UUID,
) (*ContributionSettlement, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{
		IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly,
	})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	facts, err := loadContributionJobFacts(ctx, tx, jobID)
	if err != nil {
		return nil, err
	}
	out, err := reduceContributionJobFacts(jobID, facts)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return &out, nil
}
