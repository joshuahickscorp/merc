package main

import (
	"fmt"
	"strings"
)

// Spot-quote term names. These are the units that actually cost money.
// A blended total is not auditable; each name is independently inspectable
// on SpotQuoteCostTerms and is the vocabulary a refusal must use.
const (
	spotTermSupplierFloor    = "supplier_floor"
	spotTermCompute          = "compute"
	spotTermStartup          = "startup"
	spotTermMovement         = "movement"
	spotTermVerification     = "verification"
	spotTermStorage          = "storage"
	spotTermPayment          = "payment"
	spotTermRetryRisk        = "retry_risk"
	spotTermDemandSupply     = "demand_supply"
	spotTermMercContribution = "merc_contribution"
)

// Ordered inspectable term list. Tests and auditors walk this rather than
// a pre-blended charge.
var spotQuoteTermNames = []string{
	spotTermSupplierFloor,
	spotTermCompute,
	spotTermStartup,
	spotTermMovement,
	spotTermVerification,
	spotTermStorage,
	spotTermPayment,
	spotTermRetryRisk,
	spotTermDemandSupply,
	spotTermMercContribution,
}

const (
	spotQuoteQuoted  = "quoted"
	spotQuoteRefused = "refused"
)

// Why MaximumAuthorizedChargeUSD is the bound the buyer accepted.
// Preference never changes this meaning — only which plan is priced.
const (
	spotCeilingBuyerMaxPrice      = "buyer_max_price"
	spotCeilingPlanReservedCharge = "plan_reserved_charge"
)

// SpotQuoteRequest is the buyer-facing input: preference, optional max price,
// optional deadline, and the workload geometry already expressed in compute-plan
// and catalogue units.
type SpotQuoteRequest struct {
	Preference   string
	MaxPriceUSD  float64
	DeadlineSecs int
	Schedule     EconomicSchedule
	Workload     SpotQuoteWorkload
	// ExtraTaskReserve overrides economicExtraTaskReserve(primary). Nil means
	// use that default. Zero is a degenerate plan with no retry headroom.
	ExtraTaskReserve *int
}

// SpotQuoteWorkload is the money-relevant slice of a compute plan plus the
// catalogue terms that price it. SettlementInputUnits is the same unit
// ComputePlan.SettlementInputUnits freezes.
type SpotQuoteWorkload struct {
	InputRecords          int
	InputBytes            int64
	SettlementInputUnits  float64
	EstimatedOutputTokens int64
	CataloguePricePer1K   float64
	SupplierShare         float64
	// ColdStart / SupplyTightness / StartupUSD / DemandSupplyUSD let a caller
	// (and tests) pin startup and demand-supply rather than inventing a
	// second rate card. Zero USD with ColdStart false is not-applicable.
	ColdStart       bool
	SupplyTightness float64
	StartupUSD      float64
	DemandSupplyUSD float64
}

// SpotQuoteNumbers is the three-number quote a buyer can rely on.
type SpotQuoteNumbers struct {
	EstimateUSD        float64 `json:"estimate_usd"`
	LikelyRangeLowUSD  float64 `json:"likely_range_low_usd"`
	LikelyRangeHighUSD float64 `json:"likely_range_high_usd"`
	// MaximumAuthorizedChargeUSD is the hard ceiling the buyer accepted.
	// Nothing downstream may charge above it. It is not a forecast, a hope,
	// or a preference-dependent reinterpretation of the same plan: Cheapest,
	// Balanced, and Fastest change which plan is priced, never this meaning.
	MaximumAuthorizedChargeUSD float64 `json:"maximum_authorized_charge_usd"`
	// CeilingBasis records why this ceiling is the bound the buyer accepted.
	CeilingBasis string `json:"ceiling_basis"`
}

// IsBuyerAcceptedBound is true for every well-formed quoted ceiling: the
// number is the bound the buyer accepted, not a likely outcome.
func (n SpotQuoteNumbers) IsBuyerAcceptedBound() bool {
	switch n.CeilingBasis {
	case spotCeilingBuyerMaxPrice, spotCeilingPlanReservedCharge:
		return n.MaximumAuthorizedChargeUSD > 0 &&
			n.LikelyRangeHighUSD <= n.MaximumAuthorizedChargeUSD+1e-12
	default:
		return false
	}
}

// SpotQuoteCostTerms holds every priced term as its own PricingCostComponent.
// Amounts are never pre-blended: a reader who wants compute looks at Compute,
// not at EstimateUSD.
type SpotQuoteCostTerms struct {
	SupplierFloor    PricingCostComponent `json:"supplier_floor"`
	Compute          PricingCostComponent `json:"compute"`
	Startup          PricingCostComponent `json:"startup"`
	Movement         PricingCostComponent `json:"movement"`
	Verification     PricingCostComponent `json:"verification"`
	Storage          PricingCostComponent `json:"storage"`
	Payment          PricingCostComponent `json:"payment"`
	RetryRisk        PricingCostComponent `json:"retry_risk"`
	DemandSupply     PricingCostComponent `json:"demand_supply"`
	MercContribution PricingCostComponent `json:"merc_contribution"`
}

// Component returns the named term. Unknown names are false, not a zero term.
func (t SpotQuoteCostTerms) Component(name string) (PricingCostComponent, bool) {
	switch name {
	case spotTermSupplierFloor:
		return t.SupplierFloor, true
	case spotTermCompute:
		return t.Compute, true
	case spotTermStartup:
		return t.Startup, true
	case spotTermMovement:
		return t.Movement, true
	case spotTermVerification:
		return t.Verification, true
	case spotTermStorage:
		return t.Storage, true
	case spotTermPayment:
		return t.Payment, true
	case spotTermRetryRisk:
		return t.RetryRisk, true
	case spotTermDemandSupply:
		return t.DemandSupply, true
	case spotTermMercContribution:
		return t.MercContribution, true
	default:
		return PricingCostComponent{}, false
	}
}

type spotNamedTerm struct {
	Name      string
	Component PricingCostComponent
}

func (t SpotQuoteCostTerms) named() []spotNamedTerm {
	return []spotNamedTerm{
		{spotTermSupplierFloor, t.SupplierFloor},
		{spotTermCompute, t.Compute},
		{spotTermStartup, t.Startup},
		{spotTermMovement, t.Movement},
		{spotTermVerification, t.Verification},
		{spotTermStorage, t.Storage},
		{spotTermPayment, t.Payment},
		{spotTermRetryRisk, t.RetryRisk},
		{spotTermDemandSupply, t.DemandSupply},
		{spotTermMercContribution, t.MercContribution},
	}
}

// SpotQuoteRefusal is a first-class result. Term is one of the spotQuoteTermNames
// that made the economics unacceptable.
type SpotQuoteRefusal struct {
	Term   string `json:"term"`
	Reason string `json:"reason"`
}

// SpotQuote is either a three-number quote or a named refusal.
type SpotQuote struct {
	Status     string             `json:"status"`
	Preference string             `json:"preference"`
	Plan       ComputePlan        `json:"plan,omitempty"`
	Numbers    SpotQuoteNumbers   `json:"numbers,omitempty"`
	Terms      SpotQuoteCostTerms `json:"terms,omitempty"`
	Economic   EconomicPlan       `json:"economic_plan,omitempty"`
	Refusal    *SpotQuoteRefusal  `json:"refusal,omitempty"`
}

func refusedSpotQuote(preference, term, reason string) SpotQuote {
	return SpotQuote{
		Status:     spotQuoteRefused,
		Preference: preference,
		Refusal:    &SpotQuoteRefusal{Term: term, Reason: reason},
	}
}

// ProduceSpotQuote selects a plan from preference and prices it. Preference
// never reinterprets the ceiling: MaximumAuthorizedChargeUSD is always the
// bound the buyer accepted.
func ProduceSpotQuote(req SpotQuoteRequest) SpotQuote {
	if !validWorkloadObjectives[req.Preference] {
		return refusedSpotQuote(req.Preference, spotTermCompute,
			fmt.Sprintf("preference must be %s, %s, or %s",
				workloadObjectiveCheapest, workloadObjectiveBalanced, workloadObjectiveFastest))
	}
	plan, err := selectSpotQuotedPlan(req.Preference, req.Workload)
	if err != nil {
		return refusedSpotQuote(req.Preference, spotTermCompute, err.Error())
	}
	if req.DeadlineSecs > 0 && plan.ETAP90Secs > req.DeadlineSecs {
		return refusedSpotQuote(req.Preference, spotTermDemandSupply,
			fmt.Sprintf("selected %s plan eta_p90_secs %d exceeds buyer deadline %d",
				req.Preference, plan.ETAP90Secs, req.DeadlineSecs))
	}
	return PriceSpotQuotePlan(req, plan)
}

// selectSpotQuotedPlan builds the execution geometry preference chooses.
// Task fan-out, verification work, and ETA come from the same ComputePlan
// fields the rest of settlement already uses.
func selectSpotQuotedPlan(preference string, w SpotQuoteWorkload) (ComputePlan, error) {
	if w.InputRecords <= 0 {
		return ComputePlan{}, fmt.Errorf("workload requires positive input records")
	}
	if !finiteNonNegative(w.CataloguePricePer1K) || w.CataloguePricePer1K <= 0 {
		return ComputePlan{}, fmt.Errorf("catalogue price per 1k must be finite and positive")
	}
	if !finiteNonNegative(w.SupplierShare) || w.SupplierShare <= 0 || w.SupplierShare > 1 {
		return ComputePlan{}, fmt.Errorf("supplier_share must be finite and in (0,1]")
	}
	if w.InputBytes < 0 || w.SettlementInputUnits < 0 || w.EstimatedOutputTokens < 0 {
		return ComputePlan{}, fmt.Errorf("workload geometry cannot be negative")
	}
	units := w.SettlementInputUnits
	if units == 0 {
		units = float64(w.InputRecords)
	}
	base := roundEconomicUSD(units / 1000.0 * w.CataloguePricePer1K)
	if base <= 0 {
		return ComputePlan{}, fmt.Errorf("catalogue prices settlement units at zero")
	}

	split, redundancy, honeypot, p50 := 1, 0, 1, 60
	switch preference {
	case workloadObjectiveCheapest:
		split = w.InputRecords
		redundancy, honeypot, p50 = 0, 1, 180
	case workloadObjectiveBalanced:
		split = (w.InputRecords + 1) / 2
		if split < 1 {
			split = 1
		}
		redundancy, honeypot, p50 = 1, 1, 60
	case workloadObjectiveFastest:
		split = 1
		redundancy, honeypot, p50 = 2, 1, 15
	}
	if split > w.InputRecords {
		split = w.InputRecords
	}
	if split < 1 {
		split = 1
	}
	primary := (w.InputRecords + split - 1) / split
	total := primary + redundancy + honeypot
	eta := quoteTimeFromETABands(p50, p50+p50/2, true)
	verif := 0.0
	if total > 0 {
		verif = roundEconomicUSD(base * float64(redundancy+honeypot) / float64(total))
	}
	return ComputePlan{
		ExecutionMode:           computeExecutionDistributed,
		InputRecords:            w.InputRecords,
		InputBytes:              w.InputBytes,
		SettlementInputUnits:    units,
		EstimatedOutputTokens:   w.EstimatedOutputTokens,
		SplitSize:               split,
		PrimaryTasks:            primary,
		RedundancyTasks:         redundancy,
		HoneypotTasks:           honeypot,
		TotalInitialTasks:       total,
		BaseComputeUSD:          base,
		VerificationOverheadUSD: verif,
		ETAP50Secs:              eta.P50Secs,
		ETAP90Secs:              eta.P90Secs,
		ETAWorstCaseSecs:        eta.WorstCaseSecs,
		ETASource:               "planner",
		ETAConfidenceBandMethod: eta.ConfidenceBandMethod,
	}, nil
}

// PriceSpotQuotePlan prices an already-chosen plan. Tests use this to feed
// degenerate geometries; ProduceSpotQuote is the buyer path.
func PriceSpotQuotePlan(req SpotQuoteRequest, plan ComputePlan) SpotQuote {
	pref := req.Preference
	if plan.PrimaryTasks <= 0 || plan.TotalInitialTasks <= 0 {
		return refusedSpotQuote(pref, spotTermCompute, "plan has no primary or initial tasks")
	}
	if plan.TotalInitialTasks != plan.PrimaryTasks+plan.RedundancyTasks+plan.HoneypotTasks {
		return refusedSpotQuote(pref, spotTermCompute,
			"plan total_initial_tasks does not equal primary+redundancy+honeypot")
	}
	if !finiteNonNegative(plan.BaseComputeUSD) || plan.BaseComputeUSD <= 0 {
		return refusedSpotQuote(pref, spotTermCompute, "plan base_compute_usd must be finite and positive")
	}
	if !finiteNonNegative(req.MaxPriceUSD) {
		return refusedSpotQuote(pref, spotTermCompute, "max price must be finite and non-negative")
	}
	if req.Workload.SupplierShare <= 0 || req.Workload.SupplierShare > 1 ||
		!finiteNonNegative(req.Workload.SupplierShare) {
		return refusedSpotQuote(pref, spotTermSupplierFloor, "supplier_share must be finite and in (0,1]")
	}

	extra := economicExtraTaskReserve(plan.PrimaryTasks)
	if req.ExtraTaskReserve != nil {
		extra = *req.ExtraTaskReserve
	}
	if extra < 0 {
		return refusedSpotQuote(pref, spotTermRetryRisk, "extra_task_reserve must be non-negative")
	}

	input := EconomicPlanInput{
		BaseComputeUSD:   plan.BaseComputeUSD,
		InitialTaskCount: plan.TotalInitialTasks,
		ExtraTaskReserve: extra,
		SupplierShare:    req.Workload.SupplierShare,
		FirmQuoteMaxUSD:  req.MaxPriceUSD,
	}
	econ := BuildEconomicPlan(input, req.Schedule)
	if !econ.Executable {
		return refusedSpotQuote(pref, termFromEconomicBlockReason(econ.BlockReason), econ.BlockReason)
	}
	scenario, err := fullSuccessEconomicScenario(econ)
	if err != nil {
		return refusedSpotQuote(pref, spotTermMercContribution, err.Error())
	}

	terms, err := spotQuoteTerms(req, plan, econ, scenario)
	if err != nil {
		return refusedSpotQuote(pref, spotTermMercContribution, err.Error())
	}

	// Platform extras (startup, movement, storage, retry risk, demand/supply)
	// are covered by contribution headroom. Eating the floor is a refusal, not
	// a silent loss-lead. Order is the inspection order after compute/supplier/
	// verification/payment, so the named term is the one that broke the books.
	// An unknown extra is the same class of failure: quoting it as zero would
	// be a loss-lead by accident.
	remaining := scenario.ContributionMarginUSD
	required := scenario.RequiredMarginUSD
	for _, name := range []string{
		spotTermMovement, spotTermStorage, spotTermRetryRisk,
		spotTermStartup, spotTermDemandSupply,
	} {
		comp, _ := terms.Component(name)
		if comp.Status == pricingCostUnknown {
			return refusedSpotQuote(pref, name, fmt.Sprintf(
				"%s is unknown (%s); refusing rather than treating it as free",
				name, comp.Basis))
		}
		if comp.Status != pricingCostModeled || comp.Amount <= 0 {
			continue
		}
		remaining = roundEconomicUSD(remaining - comp.Amount)
		if remaining+1e-9 < required {
			return refusedSpotQuote(pref, name, fmt.Sprintf(
				"%s of $%.6f leaves contribution $%.6f below the required $%.6f floor",
				name, comp.Amount, remaining, required))
		}
	}
	if remaining <= 0 {
		return refusedSpotQuote(pref, spotTermMercContribution,
			"named cost terms leave Merc contribution non-positive")
	}
	terms.MercContribution = modeledCost(remaining,
		"known-cost Merc contribution after supplier floor, verification, payment, startup, movement, storage, retry risk, and demand/supply")

	startupAmt := modeledAmount(terms.Startup)
	demandAmt := modeledAmount(terms.DemandSupply)
	estimate := roundEconomicUSD(econ.InitialBuyerChargeUSD + startupAmt + demandAmt)
	low := roundEconomicUSD(spotScenarioExtremum(econ, true) + startupAmt + demandAmt)
	high := roundEconomicUSD(econ.ReservedBuyerChargeUSD + startupAmt + demandAmt)
	if low > estimate {
		low = estimate
	}

	var maximum float64
	var basis string
	if req.MaxPriceUSD > 0 {
		maximum = roundEconomicUSD(req.MaxPriceUSD)
		basis = spotCeilingBuyerMaxPrice
		if estimate > maximum+1e-12 {
			over := spotTermDemandSupply
			if startupAmt >= demandAmt && startupAmt > 0 {
				over = spotTermStartup
			}
			if startupAmt == 0 && demandAmt == 0 {
				over = spotTermCompute
			}
			return refusedSpotQuote(pref, over, fmt.Sprintf(
				"estimate $%.6f exceeds the buyer-accepted maximum authorized charge $%.6f",
				estimate, maximum))
		}
		if high > maximum {
			high = maximum
		}
	} else {
		maximum = high
		basis = spotCeilingPlanReservedCharge
	}

	numbers := SpotQuoteNumbers{
		EstimateUSD:                estimate,
		LikelyRangeLowUSD:          low,
		LikelyRangeHighUSD:         high,
		MaximumAuthorizedChargeUSD: maximum,
		CeilingBasis:               basis,
	}
	if err := spotQuoteNumbersConsistent(numbers); err != nil {
		return refusedSpotQuote(pref, spotTermMercContribution, err.Error())
	}
	if !numbers.IsBuyerAcceptedBound() {
		return refusedSpotQuote(pref, spotTermMercContribution,
			"quoted ceiling is not a buyer-accepted bound")
	}
	if err := spotQuoteTermsInspectable(terms); err != nil {
		return refusedSpotQuote(pref, spotTermMercContribution, err.Error())
	}

	priced := plan
	priced.BaseComputeUSD = econ.Input.BaseComputeUSD
	return SpotQuote{
		Status:     spotQuoteQuoted,
		Preference: pref,
		Plan:       priced,
		Numbers:    numbers,
		Terms:      terms,
		Economic:   econ,
	}
}

func modeledAmount(c PricingCostComponent) float64 {
	if c.Status != pricingCostModeled {
		return 0
	}
	return c.Amount
}

func spotScenarioExtremum(plan EconomicPlan, wantMin bool) float64 {
	if len(plan.Scenarios) == 0 {
		return plan.InitialBuyerChargeUSD
	}
	best := plan.Scenarios[0].NetBilledUSD
	for _, s := range plan.Scenarios {
		if wantMin {
			if s.NetBilledUSD < best {
				best = s.NetBilledUSD
			}
		} else if s.GrossChargeUSD > best {
			best = s.GrossChargeUSD
		}
	}
	return best
}

func spotQuoteNumbersConsistent(n SpotQuoteNumbers) error {
	if !finiteNonNegative(n.EstimateUSD) || n.EstimateUSD <= 0 ||
		!finiteNonNegative(n.LikelyRangeLowUSD) ||
		!finiteNonNegative(n.LikelyRangeHighUSD) ||
		!finiteNonNegative(n.MaximumAuthorizedChargeUSD) || n.MaximumAuthorizedChargeUSD <= 0 {
		return fmt.Errorf("quote numbers must be finite and the estimate and ceiling positive")
	}
	if n.LikelyRangeLowUSD-1e-12 > n.EstimateUSD {
		return fmt.Errorf("estimate $%.6f is below likely-range low $%.6f",
			n.EstimateUSD, n.LikelyRangeLowUSD)
	}
	if n.EstimateUSD-1e-12 > n.LikelyRangeHighUSD {
		return fmt.Errorf("estimate $%.6f exceeds likely-range high $%.6f",
			n.EstimateUSD, n.LikelyRangeHighUSD)
	}
	if n.LikelyRangeHighUSD-1e-12 > n.MaximumAuthorizedChargeUSD {
		return fmt.Errorf("likely-range high $%.6f exceeds maximum authorized charge $%.6f",
			n.LikelyRangeHighUSD, n.MaximumAuthorizedChargeUSD)
	}
	return nil
}

func spotQuoteTermsInspectable(terms SpotQuoteCostTerms) error {
	for _, item := range terms.named() {
		c := item.Component
		if c.Status != pricingCostModeled &&
			c.Status != pricingCostNotApplicable &&
			c.Status != pricingCostUnknown {
			return fmt.Errorf("term %s has invalid status %q", item.Name, c.Status)
		}
		if strings.TrimSpace(c.Basis) == "" {
			return fmt.Errorf("term %s has no inspectable basis", item.Name)
		}
		if !finiteNonNegative(c.Amount) {
			return fmt.Errorf("term %s amount is not finite and non-negative", item.Name)
		}
		if c.Status != pricingCostModeled && c.Amount != 0 {
			return fmt.Errorf("term %s is %s but carries amount $%.6f",
				item.Name, c.Status, c.Amount)
		}
	}
	return nil
}

func spotQuoteTerms(
	req SpotQuoteRequest,
	plan ComputePlan,
	econ EconomicPlan,
	scenario EconomicScenario,
) (SpotQuoteCostTerms, error) {
	payout := econ.SupplierPayoutPerTaskUSD
	supplier := roundEconomicUSD(payout * float64(plan.PrimaryTasks))
	verification := roundEconomicUSD(payout * float64(plan.RedundancyTasks+plan.HoneypotTasks))
	payment := roundEconomicUSD(scenario.ProcessorFeeUSD + scenario.ControlPlaneCostUSD)

	terms := SpotQuoteCostTerms{
		SupplierFloor: modeledCost(supplier,
			"supplier floor: frozen supplier payout per task × primary task count"),
		Compute: modeledCost(econ.Input.BaseComputeUSD,
			"compute: compute-plan BaseComputeUSD from settlement units × catalogue price per 1k"),
		Payment: modeledCost(payment,
			"payment costs: economic-schedule processor fee plus allocated control-plane cost"),
		MercContribution: modeledCost(scenario.ContributionMarginUSD,
			"Merc contribution before platform extras; replaced after extras clear"),
	}
	if plan.RedundancyTasks+plan.HoneypotTasks == 0 {
		terms.Verification = notApplicableCost(
			"plan has no redundancy or honeypot tasks; no verification term applies")
	} else {
		terms.Verification = modeledCost(verification,
			"verification: frozen supplier payout per task × redundancy and honeypot task count")
	}

	terms.Startup = spotStartupTerm(req.Preference, req.Workload, econ.Input.BaseComputeUSD)
	terms.DemandSupply = spotDemandSupplyTerm(req.Preference, req.Workload, econ.Input.BaseComputeUSD)

	costSched := DefaultCostSchedule(econ.Schedule.Currency)
	storageBytes, egressBytes := declaredOutputBytesBound(plan)
	retention := currentJobObjectRetentionPolicy().Duration
	if storageBytes <= 0 || costSched.StorageNanosPerGiBMonth == 0 {
		terms.Storage = notApplicableCost(
			"compute plan declares no retained payload bytes, or storage rate is unset")
	} else {
		nanos, err := storageNanosForBytes(costSched, storageBytes, retention)
		if err != nil {
			return SpotQuoteCostTerms{}, fmt.Errorf("%s: %w", spotTermStorage, err)
		}
		terms.Storage = modeledCost(nanosToEconomicUSD(nanos),
			fmt.Sprintf("storage: %d bytes retained %s at the cost-schedule GiB-month rate",
				storageBytes, retention))
	}
	if egressBytes <= 0 || costSched.EgressNanosPerGiB == 0 {
		terms.Movement = notApplicableCost(
			"compute plan declares no result bytes, or egress rate is unset")
	} else {
		nanos, err := egressNanosForBytes(costSched, egressBytes)
		if err != nil {
			return SpotQuoteCostTerms{}, fmt.Errorf("%s: %w", spotTermMovement, err)
		}
		terms.Movement = modeledCost(nanosToEconomicUSD(nanos),
			fmt.Sprintf("movement: %d result bytes at the cost-schedule egress rate", egressBytes))
	}

	buyerNanos := usdToMicros(econ.InitialBuyerChargeUSD) * NanosPerMicro
	riskNanos := int64(0)
	if costSched.RiskReserveBasisPoints > 0 && buyerNanos > 0 {
		var rerr error
		riskNanos, rerr = riskReserveNanos(costSched, buyerNanos)
		if rerr != nil {
			return SpotQuoteCostTerms{}, fmt.Errorf("%s: %w", spotTermRetryRisk, rerr)
		}
	}
	riskUSD := nanosToEconomicUSD(riskNanos)
	if riskUSD <= 0 && econ.Input.ExtraTaskReserve == 0 {
		terms.RetryRisk = notApplicableCost(
			"no extra-task reserve and no risk-reserve policy money")
	} else {
		terms.RetryRisk = modeledCost(riskUSD, fmt.Sprintf(
			"retry risk: %d extra-task reserve in the economic plan; %d bps risk reserve on the buyer charge",
			econ.Input.ExtraTaskReserve, costSched.RiskReserveBasisPoints))
	}
	return terms, nil
}

func spotStartupTerm(preference string, w SpotQuoteWorkload, baseComputeUSD float64) PricingCostComponent {
	if w.StartupUSD > 0 {
		return modeledCost(w.StartupUSD, "declared cold-start / residency cost")
	}
	switch preference {
	case workloadObjectiveCheapest:
		return notApplicableCost(
			"CHEAPEST waits for a warm resident worker; startup is the supplier's cost, not Merc's")
	case workloadObjectiveFastest:
		amount := roundEconomicUSD(0.05 * baseComputeUSD)
		if amount <= 0 {
			amount = microsToUSD(minBillableSupplierMicros)
		}
		return modeledCost(amount,
			"FASTEST accepts cold start rather than waiting for warm supply")
	default:
		if w.ColdStart {
			amount := roundEconomicUSD(0.02 * baseComputeUSD)
			if amount <= 0 {
				amount = microsToUSD(minBillableSupplierMicros)
			}
			return modeledCost(amount, "BALANCED cold-start model load")
		}
		return notApplicableCost("BALANCED assumes warm supply; no startup term")
	}
}

func spotDemandSupplyTerm(preference string, w SpotQuoteWorkload, baseComputeUSD float64) PricingCostComponent {
	if w.DemandSupplyUSD > 0 {
		return modeledCost(w.DemandSupplyUSD, "declared demand/supply tightness surcharge")
	}
	if !finiteNonNegative(w.SupplyTightness) {
		return unknownCost("supply tightness is not finite")
	}
	switch preference {
	case workloadObjectiveCheapest:
		return notApplicableCost(
			"CHEAPEST does not bid for scarce capacity; demand/supply is not a charge")
	case workloadObjectiveFastest:
		tight := w.SupplyTightness
		if tight <= 0 {
			tight = 0.2
		}
		amount := roundEconomicUSD(tight * 0.10 * baseComputeUSD)
		if amount <= 0 {
			return notApplicableCost("FASTEST demand/supply surcharge rounded to zero")
		}
		return modeledCost(amount,
			"FASTEST bids for scarce fast capacity (demand/supply tightness)")
	default:
		if w.SupplyTightness <= 0 {
			return notApplicableCost("BALANCED demand/supply is balanced; no tightness surcharge")
		}
		amount := roundEconomicUSD(w.SupplyTightness * 0.02 * baseComputeUSD)
		if amount <= 0 {
			return notApplicableCost("BALANCED demand/supply surcharge rounded to zero")
		}
		return modeledCost(amount, "BALANCED demand/supply tightness surcharge")
	}
}

func termFromEconomicBlockReason(reason string) string {
	r := strings.ToLower(reason)
	switch {
	case strings.Contains(r, "base_compute"):
		return spotTermCompute
	case strings.Contains(r, "supplier"):
		return spotTermSupplierFloor
	case strings.Contains(r, "processor") || strings.Contains(r, "schedule"):
		return spotTermPayment
	case strings.Contains(r, "margin"):
		return spotTermMercContribution
	default:
		return spotTermMercContribution
	}
}
