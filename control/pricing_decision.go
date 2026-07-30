package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"

	"github.com/jackc/pgx/v5"
)

const (
	pricingDecisionVersion        = 1
	pricingDecisionPolicyRevision = "pricing-decision-v1"

	pricingCostModeled       = "modeled"
	pricingCostNotApplicable = "not_applicable"
	pricingCostUnknown       = "unknown"
)

// CataloguePriceAuthority is the self-contained, append-only price schedule
// entry used for one model. It prevents a quote from naming only a mutable
// models.price_per_1k cell while omitting the market board, FX and supplier
// share that made that price authoritative.
type CataloguePriceAuthority struct {
	Version                   int     `json:"version"`
	ModelID                   string  `json:"model_id"`
	JobType                   string  `json:"job_type"`
	PriceSource               string  `json:"price_source"`
	ScheduleSHA256            string  `json:"schedule_sha256"`
	ScheduleVersion           int     `json:"schedule_version"`
	ReferenceCurrency         string  `json:"reference_currency"`
	ReferencePricePer1K       float64 `json:"reference_price_per_1k"`
	SettlementCurrency        string  `json:"settlement_currency"`
	SettlementPricePer1K      float64 `json:"settlement_price_per_1k"`
	ReferenceToSettlementRate float64 `json:"reference_to_settlement_rate"`
	FXRevision                string  `json:"fx_revision"`
	BoardSHA256               string  `json:"board_sha256"`
	PriceFormula              string  `json:"price_formula"`
	SupplierShare             float64 `json:"supplier_share"`
}

// PricingCostComponent never uses an unexplained zero. A component is either
// modeled, genuinely not applicable, or explicitly unknown.
type PricingCostComponent struct {
	Status string  `json:"status"`
	Amount float64 `json:"amount"`
	Basis  string  `json:"basis"`
}

// PricingDecision is the composite economic authority accepted for a quote or
// job. Every independently valid sibling decision is bound by digest here.
// Legacy rows remain readable with no PricingDecision; they are not
// retroactively reconstructed from today's catalogue.
type PricingDecision struct {
	Version        int    `json:"version"`
	PolicyRevision string `json:"policy_revision"`
	ExecutionMode  string `json:"execution_mode"`
	Currency       string `json:"currency"`
	Tier           string `json:"tier"`

	WorkloadDecisionSHA256           string `json:"workload_decision_sha256"`
	ComputePlanSHA256                string `json:"compute_plan_sha256"`
	PlacementRequirementSHA256       string `json:"placement_requirement_sha256,omitempty"`
	EconomicPlanSHA256               string `json:"economic_plan_sha256,omitempty"`
	EconomicScheduleSHA256           string `json:"economic_schedule_sha256,omitempty"`
	OriginQuotePricingDecisionSHA256 string `json:"origin_quote_pricing_decision_sha256,omitempty"`
	OriginPricingDecisionSHA256      string `json:"origin_pricing_decision_sha256,omitempty"`

	Catalogue CataloguePriceAuthority `json:"catalogue"`

	BillableUnits                 float64 `json:"billable_units"`
	ExpectedSupplierUnitsPerSec   float64 `json:"expected_supplier_units_per_sec"`
	ExpectedSupplierSeconds       float64 `json:"expected_supplier_seconds"`
	SupplierAdmissionCeilingUSDHr float64 `json:"supplier_admission_ceiling_usd_hr"`
	ExpectedSupplierGrossUSDHr    float64 `json:"expected_supplier_gross_usd_hr"`

	BuyerPrice        float64 `json:"buyer_price"`
	MaximumBuyerPrice float64 `json:"maximum_buyer_price"`

	PrimarySupplierCost  PricingCostComponent `json:"primary_supplier_cost"`
	VerificationCost     PricingCostComponent `json:"verification_cost"`
	PaymentCost          PricingCostComponent `json:"payment_cost"`
	ControlPlaneCost     PricingCostComponent `json:"control_plane_cost"`
	StorageCost          PricingCostComponent `json:"storage_cost"`
	EgressCost           PricingCostComponent `json:"egress_cost"`
	ProviderCost         PricingCostComponent `json:"provider_cost"`
	RiskReserve          PricingCostComponent `json:"risk_reserve"`
	PlatformContribution PricingCostComponent `json:"platform_contribution"`

	Confidence  float64  `json:"confidence"`
	Assumptions []string `json:"assumptions"`
	Unknowns    []string `json:"unknowns,omitempty"`
}

func canonicalDigest(label string, value any) (string, error) {
	blob, err := json.Marshal(value)
	if err != nil {
		return "", fmt.Errorf("marshal %s: %w", label, err)
	}
	sum := sha256.Sum256(blob)
	return hex.EncodeToString(sum[:]), nil
}

func placementRequirementDigest(p PlacementRequirement) (string, error) {
	if p.Version != placementRequirementVersion {
		return "", fmt.Errorf("unsupported placement requirement version %d", p.Version)
	}
	return canonicalDigest("placement requirement", p)
}

func economicPlanDigest(p EconomicPlan) (string, error) {
	if err := ValidateEconomicPlanSnapshot(p); err != nil {
		return "", err
	}
	return canonicalDigest("economic plan", p)
}

func economicScheduleDigest(s EconomicSchedule) (string, error) {
	if reason := validateEconomicSchedule(s); reason != "" {
		return "", errors.New(reason)
	}
	return canonicalDigest("economic schedule", s)
}

func pricingDecisionDigest(p PricingDecision) (string, error) {
	if p.Version != pricingDecisionVersion {
		return "", fmt.Errorf("unsupported pricing decision version %d", p.Version)
	}
	return canonicalDigest("pricing decision", p)
}

func validateCataloguePriceAuthority(a CataloguePriceAuthority) error {
	if a.Version != cataloguePriceScheduleVersion ||
		a.ScheduleVersion != cataloguePriceScheduleVersion {
		return errors.New("catalogue authority has an unsupported schedule version")
	}
	if a.ModelID == "" || a.JobType == "" || a.PriceSource != "market_board" {
		return errors.New("catalogue authority lacks governed model identity")
	}
	if !validSHA256(a.ScheduleSHA256) || !validSHA256(a.BoardSHA256) {
		return errors.New("catalogue authority lacks valid schedule and board digests")
	}
	if a.ReferenceCurrency != catalogueReferenceCurrency ||
		a.SettlementCurrency == "" ||
		a.FXRevision == "" ||
		strings.TrimSpace(a.PriceFormula) == "" {
		return errors.New("catalogue authority lacks currency, FX, or formula provenance")
	}
	currency, err := ParseCurrency(a.SettlementCurrency)
	if err != nil || currency.Code() != a.SettlementCurrency {
		return errors.New("catalogue authority settlement currency is unsupported")
	}
	for name, value := range map[string]float64{
		"reference price":  a.ReferencePricePer1K,
		"settlement price": a.SettlementPricePer1K,
		"FX rate":          a.ReferenceToSettlementRate,
		"supplier share":   a.SupplierShare,
	} {
		if !finiteNonNegative(value) || value <= 0 {
			return fmt.Errorf("catalogue authority %s must be finite and positive", name)
		}
	}
	if a.SupplierShare > 1 {
		return errors.New("catalogue authority supplier share exceeds one")
	}
	wantSettlement := ceilPricePer1K(a.ReferencePricePer1K * a.ReferenceToSettlementRate)
	if math.Abs(a.SettlementPricePer1K-wantSettlement) > 0.0000000001 {
		return errors.New("catalogue settlement price does not match frozen reference price and FX")
	}
	return nil
}

// LoadCataloguePriceAuthority reads the current model pointer and its matching
// append-only schedule/history rows in one statement. A seed or partially
// migrated price is not accepted as production pricing authority.
func (s *Store) LoadCataloguePriceAuthority(ctx context.Context, modelID string) (CataloguePriceAuthority, error) {
	var a CataloguePriceAuthority
	err := s.pool.QueryRow(ctx, `
		SELECT s.version,m.id,COALESCE(m.job_type,''),COALESCE(m.price_source,''),
		       s.sha256,COALESCE(m.price_schedule_version,0),
		       s.reference_currency,h.reference_price_per_1k::float8,
		       s.settlement_currency,h.price_per_1k::float8,
		       s.reference_to_settlement_rate,s.fx_revision,s.board_sha256,
		       h.price_formula,s.supplier_share
		  FROM models m
		  JOIN catalogue_price_schedules s ON s.sha256=m.price_schedule_sha256
		  JOIN model_price_history h
		    ON h.schedule_sha256=s.sha256 AND h.model_id=m.id
		 WHERE m.id=$1
		   AND m.price_source='market_board'
		   AND m.price_per_1k=h.price_per_1k
		   AND m.price_reference_per_1k=h.reference_price_per_1k
		   AND m.price_currency=h.price_currency
		   AND m.price_formula=h.price_formula`,
		modelID,
	).Scan(
		&a.Version, &a.ModelID, &a.JobType, &a.PriceSource,
		&a.ScheduleSHA256, &a.ScheduleVersion,
		&a.ReferenceCurrency, &a.ReferencePricePer1K,
		&a.SettlementCurrency, &a.SettlementPricePer1K,
		&a.ReferenceToSettlementRate, &a.FXRevision, &a.BoardSHA256,
		&a.PriceFormula, &a.SupplierShare,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return CataloguePriceAuthority{}, fmt.Errorf(
			"model %s has no complete append-only catalogue price authority", modelID,
		)
	}
	if err != nil {
		return CataloguePriceAuthority{}, err
	}
	if err := validateCataloguePriceAuthority(a); err != nil {
		return CataloguePriceAuthority{}, fmt.Errorf("model %s catalogue authority: %w", modelID, err)
	}
	if err := RequireSettlementCurrency(a.SettlementCurrency); err != nil {
		return CataloguePriceAuthority{}, fmt.Errorf(
			"model %s catalogue authority is not current settlement currency: %w",
			modelID, err,
		)
	}
	return a, nil
}

// selectCataloguePriceAuthority makes the quote pinning rule executable: a
// bound submission must not consult the mutable models pointer at all.
func selectCataloguePriceAuthority(
	bound *boundQuote,
	unbound func() (CataloguePriceAuthority, error),
) (CataloguePriceAuthority, error) {
	if bound != nil {
		if err := validateCataloguePriceAuthority(bound.Pricing.Catalogue); err != nil {
			return CataloguePriceAuthority{}, fmt.Errorf(
				"bound quote lacks valid catalogue authority: %w", err,
			)
		}
		return bound.Pricing.Catalogue, nil
	}
	return unbound()
}

func estimateJobSettlementWithAuthority(
	a CataloguePriceAuthority,
	jobType string,
	inputBytesLen, nLines int,
	maxTokens uint32,
	tier string,
) (float64, error) {
	if err := validateCataloguePriceAuthority(a); err != nil {
		return 0, err
	}
	if a.JobType != jobType {
		return 0, fmt.Errorf("catalogue job type %s does not match %s", a.JobType, jobType)
	}
	units := settlementInputUnitsForGeometry(nLines, int64(inputBytesLen))
	if generativeJobType(jobType) && nLines > 0 {
		outTokensPerRecord := maxTokens
		if outTokensPerRecord == 0 {
			outTokensPerRecord = defaultQuoteMaxTokens
		}
		units += float64(nLines) * float64(outTokensPerRecord)
	}
	rounded := roundUSD(units / 1000 * a.SettlementPricePer1K * tierMultiplier(tier))
	if rounded == 0 && units > 0 {
		return microsToUSD(1), nil
	}
	return rounded, nil
}

// pricingBillableUnitsForComputePlan keeps historical pricing decisions
// verifiable while making every newly written (v3) decision use the exact
// input-side units that created the catalogue money estimate. Version-1 and
// version-2 receipts retain their historical body/whole-input presentation;
// they must not be silently reinterpreted or re-priced after acceptance.
func pricingBillableUnitsForComputePlan(compute ComputePlan) float64 {
	input := float64(compute.EstimatedInputTokens)
	if compute.Version == computePlanVersion {
		input = compute.SettlementInputUnits
	}
	return input + float64(compute.EstimatedOutputTokens)
}

// supplierAdmissionCeilingUSDHr is a modeled ask ceiling used solely by the
// scheduler's min-payout eligibility gate. It is not a promise of realized
// hourly earnings: actual payment follows accepted task units.
func supplierAdmissionCeilingUSDHr(a CataloguePriceAuthority, jobType, tier string) (float64, error) {
	if err := validateCataloguePriceAuthority(a); err != nil {
		return 0, err
	}
	if a.JobType != jobType {
		return 0, errors.New("catalogue job type does not match placement job type")
	}
	return throughputOf(jobType) * 3600 / 1000 *
		a.ReferencePricePer1K * a.SupplierShare * tierMultiplier(tier), nil
}

func modeledCost(amount float64, basis string) PricingCostComponent {
	return PricingCostComponent{Status: pricingCostModeled, Amount: roundEconomicUSD(amount), Basis: basis}
}

func unknownCost(basis string) PricingCostComponent {
	return PricingCostComponent{Status: pricingCostUnknown, Basis: basis}
}

func notApplicableCost(basis string) PricingCostComponent {
	return PricingCostComponent{Status: pricingCostNotApplicable, Basis: basis}
}

func fullSuccessEconomicScenario(plan EconomicPlan) (EconomicScenario, error) {
	for _, scenario := range plan.Scenarios {
		if scenario.Name == "full_success_sla_met" {
			return scenario, nil
		}
	}
	return EconomicScenario{}, errors.New("economic plan lacks full_success_sla_met scenario")
}

func newDistributedPricingDecision(
	workload WorkloadDecision,
	compute ComputePlan,
	placement PlacementRequirement,
	economic EconomicPlan,
	catalogue CataloguePriceAuthority,
	tier string,
	originQuotePricingSHA string,
) (PricingDecision, error) {
	if err := ValidateComputePlanEconomicSnapshot(compute, workload, economic); err != nil {
		return PricingDecision{}, err
	}
	if err := validatePlacementRequirement(placement, workload); err != nil {
		return PricingDecision{}, err
	}
	if err := validateCataloguePriceAuthority(catalogue); err != nil {
		return PricingDecision{}, err
	}
	if catalogue.ModelID != workload.Binding.Model.Ref ||
		catalogue.JobType != workload.RuntimeJobType ||
		catalogue.SettlementCurrency != economic.Schedule.Currency ||
		tier != workload.Binding.Tier ||
		math.Abs(catalogue.SupplierShare-economic.Input.SupplierShare) > 0.000000001 {
		return PricingDecision{}, errors.New("catalogue, workload and economic authority disagree")
	}
	if originQuotePricingSHA != "" && !validSHA256(originQuotePricingSHA) {
		return PricingDecision{}, errors.New("origin quote pricing digest is invalid")
	}
	derivedCeiling, err := supplierAdmissionCeilingUSDHr(catalogue, workload.RuntimeJobType, tier)
	if err != nil {
		return PricingDecision{}, err
	}
	if math.Abs(float64(placement.OfferedRateUsdHr)-derivedCeiling) > 0.000001 {
		return PricingDecision{}, errors.New("placement supplier admission ceiling was not derived from catalogue authority")
	}
	// The scheduler and wire contract use float32. Freeze that exact operational
	// value in pricing too so the receipt never displays a nearby float64 that
	// was not actually used for admission.
	ceiling := float64(placement.OfferedRateUsdHr)
	workloadSHA, err := workloadDecisionDigest(workload)
	if err != nil {
		return PricingDecision{}, err
	}
	computeSHA, err := computePlanDigest(compute)
	if err != nil {
		return PricingDecision{}, err
	}
	placementSHA, err := placementRequirementDigest(placement)
	if err != nil {
		return PricingDecision{}, err
	}
	economicSHA, err := economicPlanDigest(economic)
	if err != nil {
		return PricingDecision{}, err
	}
	scheduleSHA, err := economicScheduleDigest(economic.Schedule)
	if err != nil {
		return PricingDecision{}, err
	}
	scenario, err := fullSuccessEconomicScenario(economic)
	if err != nil {
		return PricingDecision{}, err
	}
	primarySupplier := economic.SupplierPayoutPerTaskUSD * float64(compute.PrimaryTasks)
	verification := economic.SupplierPayoutPerTaskUSD *
		float64(compute.RedundancyTasks+compute.HoneypotTasks)
	billableUnits := pricingBillableUnitsForComputePlan(compute)
	unitsPerSec := throughputOf(workload.RuntimeJobType)
	expectedSeconds := 0.0
	if compute.PrimaryTasks > 0 && unitsPerSec > 0 {
		expectedSeconds = billableUnits / unitsPerSec *
			float64(compute.TotalInitialTasks) / float64(compute.PrimaryTasks)
	}
	expectedGrossUSDHr := 0.0
	if expectedSeconds > 0 {
		settlementSupplier := primarySupplier + verification
		expectedGrossUSDHr = settlementSupplier /
			catalogue.ReferenceToSettlementRate / expectedSeconds * 3600
	}
	out := PricingDecision{
		Version: pricingDecisionVersion, PolicyRevision: pricingDecisionPolicyRevision,
		ExecutionMode: computeExecutionDistributed,
		Currency:      economic.Schedule.Currency, Tier: tier,
		WorkloadDecisionSHA256: workloadSHA, ComputePlanSHA256: computeSHA,
		PlacementRequirementSHA256: placementSHA,
		EconomicPlanSHA256:         economicSHA, EconomicScheduleSHA256: scheduleSHA,
		OriginQuotePricingDecisionSHA256: originQuotePricingSHA,
		Catalogue:                        catalogue,
		BillableUnits:                    billableUnits,
		ExpectedSupplierUnitsPerSec:      unitsPerSec,
		ExpectedSupplierSeconds:          expectedSeconds,
		SupplierAdmissionCeilingUSDHr:    ceiling,
		ExpectedSupplierGrossUSDHr:       expectedGrossUSDHr,
		BuyerPrice:                       economic.InitialBuyerChargeUSD,
		MaximumBuyerPrice:                economic.ReservedBuyerChargeUSD,
		PrimarySupplierCost: modeledCost(primarySupplier,
			"frozen supplier payout per task × primary task count"),
		VerificationCost: modeledCost(verification,
			"frozen supplier payout per task × redundancy and honeypot task count"),
		PaymentCost: modeledCost(scenario.ProcessorFeeUSD,
			"economic schedule processor percentage and allocated fixed fee"),
		ControlPlaneCost: modeledCost(scenario.ControlPlaneCostUSD,
			"economic schedule per-task control-plane cost"),
		StorageCost: unknownCost(
			"no independently metered object-storage cost is attributed at acceptance"),
		EgressCost: unknownCost(
			"result egress destination and billable bytes are unknown at acceptance"),
		ProviderCost: unknownCost(
			"worker/provider energy and depreciation are not independently metered"),
		RiskReserve: unknownCost(
			"no independently calibrated loss reserve is available"),
		PlatformContribution: modeledCost(scenario.ContributionMarginUSD,
			"buyer price less modeled supplier, processor and control-plane costs"),
		Confidence: compute.Confidence,
		Assumptions: []string{
			"supplier admission uses modeled static throughput and the USD reference schedule",
			"actual supplier liability is frozen per accepted task, not by elapsed wall-clock hour",
			"unknown cost components are not silently treated as modeled zero",
		},
		Unknowns: []string{
			"storage cost", "egress cost", "provider energy and depreciation", "risk reserve",
		},
	}
	return out, validatePricingCostShape(out)
}

func newExactReusePricingDecision(
	workload WorkloadDecision,
	compute ComputePlan,
	catalogue CataloguePriceAuthority,
	tier string,
	buyerCharge float64,
	originPricingSHA string,
) (PricingDecision, error) {
	if err := ValidateFrozenComputePlanSnapshot(compute, workload); err != nil {
		return PricingDecision{}, err
	}
	if compute.ExecutionMode != computeExecutionExactReuse {
		return PricingDecision{}, errors.New("exact-reuse pricing requires exact-reuse compute")
	}
	if err := validateCataloguePriceAuthority(catalogue); err != nil {
		return PricingDecision{}, err
	}
	if catalogue.ModelID != workload.Binding.Model.Ref ||
		catalogue.JobType != workload.RuntimeJobType ||
		catalogue.SettlementCurrency != SettlementCurrencyCode() ||
		tier != workload.Binding.Tier {
		return PricingDecision{}, errors.New("exact-reuse catalogue and workload authority disagree")
	}
	if buyerCharge <= 0 || compute.BaseComputeUSD != roundEconomicUSD(buyerCharge) {
		return PricingDecision{}, errors.New("exact-reuse buyer charge does not match compute authority")
	}
	if originPricingSHA != "" && !validSHA256(originPricingSHA) {
		return PricingDecision{}, errors.New("exact-reuse origin pricing digest is invalid")
	}
	workloadSHA, err := workloadDecisionDigest(workload)
	if err != nil {
		return PricingDecision{}, err
	}
	computeSHA, err := computePlanDigest(compute)
	if err != nil {
		return PricingDecision{}, err
	}
	out := PricingDecision{
		Version: pricingDecisionVersion, PolicyRevision: pricingDecisionPolicyRevision,
		ExecutionMode: computeExecutionExactReuse,
		Currency:      catalogue.SettlementCurrency, Tier: tier,
		WorkloadDecisionSHA256: workloadSHA, ComputePlanSHA256: computeSHA,
		OriginPricingDecisionSHA256: originPricingSHA,
		Catalogue:                   catalogue,
		BillableUnits:               pricingBillableUnitsForComputePlan(compute),
		BuyerPrice:                  buyerCharge, MaximumBuyerPrice: buyerCharge,
		PrimarySupplierCost: notApplicableCost(
			"no physical supplier executes an exact-result reuse delivery"),
		VerificationCost: notApplicableCost(
			"the delivered object reuses an already verified exact result"),
		PaymentCost: unknownCost(
			"actual processor allocation is reconciled after collection"),
		ControlPlaneCost: unknownCost(
			"reuse lookup and materialization are not independently metered"),
		StorageCost: unknownCost(
			"cached-object storage is not independently attributed"),
		EgressCost: unknownCost(
			"result egress is not known at acceptance"),
		ProviderCost: notApplicableCost(
			"no external compute provider executes the reused workload"),
		RiskReserve: unknownCost(
			"no independently calibrated loss reserve is available"),
		PlatformContribution: modeledCost(buyerCharge,
			"gross reuse price before unknown payment, storage, egress and control costs"),
		Confidence: 1,
		Assumptions: []string{
			"exact identity includes the complete frozen workload decision",
			"physical supplier work and verification fan-out are zero for this delivery",
		},
		Unknowns: []string{
			"payment cost", "control-plane cost", "storage cost", "egress cost", "risk reserve",
		},
	}
	return out, validatePricingCostShape(out)
}

func validatePricingCostShape(p PricingDecision) error {
	if p.Version != pricingDecisionVersion || p.PolicyRevision != pricingDecisionPolicyRevision {
		return errors.New("unsupported pricing decision")
	}
	currency, err := ParseCurrency(p.Currency)
	if err != nil || currency.Code() != p.Currency {
		return errors.New("pricing decision currency is unsupported")
	}
	if p.Currency != p.Catalogue.SettlementCurrency ||
		!validSHA256(p.WorkloadDecisionSHA256) ||
		!validSHA256(p.ComputePlanSHA256) {
		return errors.New("pricing decision lacks core digest or currency authority")
	}
	if !finiteNonNegative(p.BuyerPrice) || p.BuyerPrice <= 0 ||
		!finiteNonNegative(p.MaximumBuyerPrice) ||
		p.MaximumBuyerPrice < p.BuyerPrice ||
		!finiteNonNegative(p.Confidence) || p.Confidence > 1 {
		return errors.New("pricing decision has invalid price or confidence")
	}
	components := []PricingCostComponent{
		p.PrimarySupplierCost, p.VerificationCost, p.PaymentCost,
		p.ControlPlaneCost, p.StorageCost, p.EgressCost, p.ProviderCost,
		p.RiskReserve, p.PlatformContribution,
	}
	for _, component := range components {
		if component.Status != pricingCostModeled &&
			component.Status != pricingCostNotApplicable &&
			component.Status != pricingCostUnknown {
			return errors.New("pricing cost component has invalid status")
		}
		if !finiteNonNegative(component.Amount) || strings.TrimSpace(component.Basis) == "" {
			return errors.New("pricing cost component lacks a finite amount or basis")
		}
		if component.Status != pricingCostModeled && component.Amount != 0 {
			return errors.New("unknown or not-applicable pricing cost cannot carry a modeled amount")
		}
	}
	switch p.ExecutionMode {
	case computeExecutionDistributed:
		if !validSHA256(p.PlacementRequirementSHA256) ||
			!validSHA256(p.EconomicPlanSHA256) ||
			!validSHA256(p.EconomicScheduleSHA256) ||
			p.ExpectedSupplierUnitsPerSec <= 0 ||
			p.ExpectedSupplierSeconds <= 0 ||
			p.SupplierAdmissionCeilingUSDHr <= 0 ||
			p.ExpectedSupplierGrossUSDHr+0.000001 < p.SupplierAdmissionCeilingUSDHr {
			return errors.New("physical pricing decision lacks executable composite authority")
		}
		conserved := roundEconomicUSD(
			p.PrimarySupplierCost.Amount + p.VerificationCost.Amount +
				p.PaymentCost.Amount + p.ControlPlaneCost.Amount +
				p.PlatformContribution.Amount,
		)
		if math.Abs(conserved-p.BuyerPrice) > 0.000002 {
			return errors.New("physical pricing decision does not conserve modeled buyer price")
		}
	case computeExecutionExactReuse:
		if p.PlacementRequirementSHA256 != "" ||
			p.EconomicPlanSHA256 != "" ||
			p.EconomicScheduleSHA256 != "" ||
			p.ExpectedSupplierUnitsPerSec != 0 ||
			p.ExpectedSupplierSeconds != 0 ||
			p.SupplierAdmissionCeilingUSDHr != 0 ||
			p.ExpectedSupplierGrossUSDHr != 0 ||
			p.PrimarySupplierCost.Status != pricingCostNotApplicable ||
			p.VerificationCost.Status != pricingCostNotApplicable {
			return errors.New("exact-reuse pricing falsely attributes physical execution")
		}
	default:
		return fmt.Errorf("unknown pricing execution mode %q", p.ExecutionMode)
	}
	return nil
}

func ValidateDistributedPricingDecisionSnapshot(
	decision PricingDecision,
	workload WorkloadDecision,
	compute ComputePlan,
	placement PlacementRequirement,
	economic EconomicPlan,
) error {
	rebuilt, err := newDistributedPricingDecision(
		workload, compute, placement, economic, decision.Catalogue, decision.Tier,
		decision.OriginQuotePricingDecisionSHA256,
	)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(decision, rebuilt) {
		return errors.New("pricing decision does not match its deterministic composite authority")
	}
	return nil
}

func ValidateExactReusePricingDecisionSnapshot(
	decision PricingDecision,
	workload WorkloadDecision,
	compute ComputePlan,
) error {
	rebuilt, err := newExactReusePricingDecision(
		workload, compute, decision.Catalogue, decision.Tier,
		decision.BuyerPrice, decision.OriginPricingDecisionSHA256,
	)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(decision, rebuilt) {
		return errors.New("exact-reuse pricing decision does not match its deterministic authority")
	}
	return nil
}
