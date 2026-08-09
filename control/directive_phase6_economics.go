package main

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
)

// Phase 6 directive physical economics.
//
// The programme requires physical cost to include actual duration, utilization,
// provider/supplier rate, startup and residency, verification, retries, storage
// and egress, payment allocation, energy, and refund risk — then to compute:
//
//	cost per verified outcome
//	joules per verified outcome
//	buyer savings
//	supplier contribution
//	Merc true net contribution
//
// Primary system metric:
//
//	verified successful outcomes per dollar per joule inside SLA
//
// This file is the honesty layer for those claims. Every named category is
// KNOWN (with a bound source) or UNKNOWN (with what would make it known). No
// number is emitted without a source. Gross platform ledger rows are a
// different type from true net and cannot be read as true net.
//
// Historical PricingDecision bodies may contain
// FixedPoint.TrueNetContributionNanos, but PricingDecision is accepted-forecast
// authority and that raw field is not settlement. Batch-job true net is emitted
// only by a FINAL ContributionSettlement. Neither authority alone satisfies the
// broader Phase 6 physical-cost inventory (utilization, startup/residency,
// measured package energy, measured refund cohort, etc.). This file is the
// programme-level gate that refuses to quote the primary metric until every
// directive category is known.

// CostCategoryKnowledge is how a directive cost category stands today.
type CostCategoryKnowledge string

const (
	// CategoryKnown: a bound measurement or governed rate is cited.
	CategoryKnown CostCategoryKnowledge = "KNOWN"
	// CategoryUnknown: the category applies and no attributable value exists.
	// Distinct from zero: zero is an amount, unknown is a refusal to invent one.
	CategoryUnknown CostCategoryKnowledge = "UNKNOWN"
	// CategoryNotApplicable: the category does not apply to this placement.
	CategoryNotApplicable CostCategoryKnowledge = "NOT_APPLICABLE"
	// CategoryDefaulted: a published policy rate with provenance, not a host meter.
	CategoryDefaulted CostCategoryKnowledge = "DEFAULTED"
	// CategoryAssumed: an estimate with no measurement path (must never present as measured).
	CategoryAssumed CostCategoryKnowledge = "ASSUMED"
)

// Directive cost category names — stable wire identifiers matching the programme list.
const (
	dirCatActualDuration       = "actual_duration"
	dirCatUtilization          = "utilization"
	dirCatProviderSupplierRate = "provider_supplier_rate"
	dirCatStartupResidency     = "startup_and_residency"
	dirCatVerification         = "verification"
	dirCatRetries              = "retries"
	dirCatStorageEgress        = "storage_and_egress"
	dirCatPaymentAllocation    = "payment_allocation"
	dirCatEnergy               = "energy"
	dirCatRefundRisk           = "refund_risk"
)

// DirectiveCostCategory is one named physical-cost term with knowledge state.
type DirectiveCostCategory struct {
	Name         string                `json:"name"`
	Knowledge    CostCategoryKnowledge `json:"knowledge"`
	Source       string                `json:"source,omitempty"`
	WouldRequire string                `json:"would_require,omitempty"`
	Notes        string                `json:"notes,omitempty"`
	// MoneyNanos is set only when Knowledge is KNOWN or DEFAULTED and a
	// money amount is attributed. Zero with Knowledge==KNOWN is valid when
	// the term is a non-money base (e.g. duration) — see NonMoney.
	MoneyNanos int64 `json:"money_nanos,omitempty"`
	// NonMoney carries the measured non-money value (seconds, fraction, joules).
	NonMoney     float64 `json:"non_money,omitempty"`
	NonMoneyUnit string  `json:"non_money_unit,omitempty"`
}

// UnavailableQuantity is intentionally not a number.
//
// There is no Amount, Nanos, USD, or Float64 field and no method that returns
// an economic quantity. JSON encodes as status=unavailable. Callers that need
// a number must switch on Kind and refuse — they cannot silently coerce this
// type into a float.
type UnavailableQuantity struct {
	Status             string   `json:"status"` // always "unavailable"
	Kind               string   `json:"kind"`
	Reason             string   `json:"reason"`
	BlockingCategories []string `json:"blocking_categories,omitempty"`
}

// unavailable is the only constructor; Status is always "unavailable".
func unavailable(kind, reason string, blocking []string) UnavailableQuantity {
	return UnavailableQuantity{
		Status:             "unavailable",
		Kind:               kind,
		Reason:             reason,
		BlockingCategories: append([]string(nil), blocking...),
	}
}

// KnownQuantity is a number with mandatory provenance. Source must cite a
// bound receipt path or a governed constant with its file and name.
type KnownQuantity struct {
	Status string  `json:"status"` // always "known"
	Value  float64 `json:"value"`
	Unit   string  `json:"unit"`
	Source string  `json:"source"`
	Basis  string  `json:"basis"`
	// Knowledge qualifies the dollar conversion or measurement path when the
	// number itself is derived (e.g. joules measured, electricity defaulted).
	Knowledge CostCategoryKnowledge `json:"knowledge,omitempty"`
}

func knownQty(value float64, unit, source, basis string, k CostCategoryKnowledge) KnownQuantity {
	return KnownQuantity{
		Status:    "known",
		Value:     value,
		Unit:      unit,
		Source:    source,
		Basis:     basis,
		Knowledge: k,
	}
}

// GrossPlatformLedgerRow is the settlement residual after supplier entitlement
// (and any known variable costs already in the fixed-point equation).
//
// It is NEVER true net contribution. The anti-gaming law forbids presenting a
// gross platform row as net. The type name and Label field make that impossible
// to miss in JSON; there is no TrueNet alias and no conversion method to
// MercTrueNetContribution.
type GrossPlatformLedgerRow struct {
	Label    string `json:"label"` // always gross_platform_ledger_row_not_true_net
	Nanos    int64  `json:"nanos"`
	Currency string `json:"currency"`
	Source   string `json:"source"`
	Basis    string `json:"basis"`
	// Explicit refusal to be read as true net.
	NotTrueNet string `json:"not_true_net"`
}

// MercTrueNetContribution is either unavailable or (in a future complete
// inventory) a known money amount. When Unavailable is set, Available is nil
// and there is no numeric field on the outer struct that a careless marshaler
// could quote as profit.
type MercTrueNetContribution struct {
	// Available is non-nil only when every directive category is KNOWN or
	// NOT_APPLICABLE and a buyer charge is present. Today this is always nil.
	Available *KnownQuantity `json:"available,omitempty"`
	// Unavailable is set whenever any named category is UNKNOWN or ASSUMED.
	Unavailable *UnavailableQuantity `json:"unavailable,omitempty"`
}

// IsAvailable reports whether true net can be read as a number. Prefer this
// over inspecting Available directly so call sites stay greppable.
func (m MercTrueNetContribution) IsAvailable() bool {
	return m.Available != nil && m.Unavailable == nil
}

// BuyerSavings is the counterfactual "what the buyer would have paid elsewhere
// minus what they paid here". Without a bound external price, it is unavailable.
type BuyerSavings struct {
	Available   *KnownQuantity       `json:"available,omitempty"`
	Unavailable *UnavailableQuantity `json:"unavailable,omitempty"`
}

// SupplierContribution is the supplier's ledger credit for verified work on a
// money path. It is not a claim about the supplier's physical cost stack.
type SupplierContribution struct {
	Available   *KnownQuantity       `json:"available,omitempty"`
	Unavailable *UnavailableQuantity `json:"unavailable,omitempty"`
	// PhysicalCostOfSupplier is always unavailable today: we credit entitlement,
	// we do not observe the supplier's energy/depreciation bill.
	PhysicalCostOfSupplier UnavailableQuantity `json:"physical_cost_of_supplier"`
}

// PrimarySystemMetric is
//
//	verified successful outcomes / (dollar × joule) inside SLA
//
// Quoting a number before every term is real is refused in the type: there is
// no Value field when Unavailable is set.
type PrimarySystemMetric struct {
	Name        string               `json:"name"`
	Available   *KnownQuantity       `json:"available,omitempty"`
	Unavailable *UnavailableQuantity `json:"unavailable,omitempty"`
	// TermStatus explains each factor of the product.
	TermStatus PrimaryMetricTerms `json:"term_status"`
}

// PrimaryMetricTerms names which factor of the primary metric is ready.
type PrimaryMetricTerms struct {
	VerifiedOutcomes string `json:"verified_outcomes"`
	DollarCost       string `json:"dollar_cost_per_outcome"`
	Joules           string `json:"joules_per_outcome"`
	InsideSLA        string `json:"inside_sla"`
}

const primaryMetricName = "verified_successful_outcomes_per_dollar_per_joule_inside_sla"

// Bound energy authority for the local Metal path (GPU-domain IOReport).
const (
	boundEnergyAuthorityPath = "evidence/perf/ioreport-gpu-energy-authority.json"
	// Canonical joules/outcome from that BOUND receipt. Re-read from disk in
	// LoadBoundMetalEnergyAuthority; this constant is the pin the test asserts
	// against so a silent rewrite of the JSON fails closed.
	boundJoulesPerVerifiedOutcome = 0.121724572
	boundEnergyVerifiedOutcomes   = 512
	boundEnergyLoadWindowSeconds  = 2.5734
	boundEnergyCurrencyAssumption = "usd" // electricity default is USD-denominated
)

// Bound money-conservation receipt (128 deliveries → 1 physical execution).
// Used only for supplier contribution / gross platform row citations — never
// as true net.
const (
	boundMoneyConservationPath = "evidence/reuse/public-path-128-to-1.json"
	// Cluster totals from that BOUND receipt (CAD settlement).
	boundMoneyBuyerChargeNanosTotal           int64 = 1_493_280
	boundMoneySupplierPayableNanosTotal       int64 = 20_160
	boundMoneyKnownCostContributionNanosTotal int64 = 1_473_120
	boundMoneyCurrency                              = "cad"
	boundMoneyPhysicalExecutions                    = 1
	boundMoneyDeliveries                            = 128
)

// Phase6EconomicsReceipt is the programme-facing artifact for what is and is
// not computable today on the local Metal path plus the money-conservation path.
type Phase6EconomicsReceipt struct {
	SchemaVersion int    `json:"schema_version"`
	Kind          string `json:"kind"`
	Title         string `json:"title"`

	// Categories is the full directive inventory for the Metal local placement.
	Categories []DirectiveCostCategory `json:"categories"`

	// Computable metrics.
	JoulesPerVerifiedOutcome *KnownQuantity       `json:"joules_per_verified_outcome"`
	CostPerVerifiedOutcome   *UnavailableQuantity `json:"cost_per_verified_outcome"`
	// EnergyUSDPerVerifiedOutcome is a PARTIAL dollar figure: GPU-domain joules
	// × defaulted electricity. It is not full physical cost and must not be
	// quoted as cost_per_verified_outcome.
	EnergyUSDPerVerifiedOutcome *KnownQuantity `json:"energy_usd_per_verified_outcome_partial"`

	BuyerSavings         BuyerSavings            `json:"buyer_savings"`
	SupplierContribution SupplierContribution    `json:"supplier_contribution"`
	MercTrueNet          MercTrueNetContribution `json:"merc_true_net_contribution"`
	GrossPlatformRow     *GrossPlatformLedgerRow `json:"gross_platform_ledger_row,omitempty"`
	PrimaryMetric        PrimarySystemMetric     `json:"primary_system_metric"`

	// Honest one-line programme answers.
	MercTrueNetComputableToday bool   `json:"merc_true_net_computable_today"`
	PrimaryMetricQuotableToday bool   `json:"primary_metric_quotable_today"`
	HonestSummary              string `json:"honest_summary"`

	// Scope boundary — what this receipt covers and excludes.
	Scope Phase6Scope `json:"scope"`
}

// Phase6Scope states the measurement boundary so GPU-domain joules are not
// confused with package or wall energy, and money-path gross is not net.
type Phase6Scope struct {
	Placement              string   `json:"placement"`
	EnergyBoundary         string   `json:"energy_boundary"`
	EnergyExcludes         []string `json:"energy_excludes"`
	NoCUDAInvented         bool     `json:"no_cuda_cost_invented"`
	GrossIsNotTrueNet      bool     `json:"gross_platform_row_is_not_true_net"`
	PricingDecisionTrueNet string   `json:"pricing_decision_true_net_distinction"`
}

// BuildMetalLocalPhase6Economics constructs the programme receipt for the
// local llama.cpp/Metal path using bound energy and money-conservation sources.
// It never invents a CUDA cost and never presents gross as true net.
func BuildMetalLocalPhase6Economics() Phase6EconomicsReceipt {
	cats := metalLocalCategoryInventory()
	blocking := unknownOrAssumedNames(cats)

	joules := knownQty(
		boundJoulesPerVerifiedOutcome,
		"joules_per_verified_outcome",
		boundEnergyAuthorityPath+"#joules_per_verified_outcome",
		"IOReport GPU Energy channel (AGX on-chip domain only); BOUND measured_energy_authority; "+
			"512 completion_tokens over load_window; not package, not wall-plug",
		CategoryKnown,
	)

	// Energy $ conversion: joules / 3.6e6 * defaultElectricityUSDPerKWh.
	// Electricity rate is policy-defaulted (same constant as control/pricing.go),
	// so the dollar figure is DEFAULTED even though joules are measured.
	energyUSD := boundJoulesPerVerifiedOutcome / 3.6e6 * defaultElectricityUSDPerKWh
	energyUSDQty := knownQty(
		energyUSD,
		"usd_per_verified_outcome",
		boundEnergyAuthorityPath+" + control/pricing.go:defaultElectricityUSDPerKWh",
		fmt.Sprintf(
			"GPU-domain joules (%.9f J) × defaulted electricity $%.2f/kWh; "+
				"NOT full cost_per_verified_outcome; package/CPU/DRAM/ANE/startup/storage/payment/refund excluded",
			boundJoulesPerVerifiedOutcome, defaultElectricityUSDPerKWh,
		),
		CategoryDefaulted,
	)

	fullCostBlocked := unavailable(
		"cost_per_verified_outcome",
		"full physical cost per verified outcome requires every directive category; "+
			"only GPU-domain energy is measured on this path — see energy_usd_per_verified_outcome_partial for the partial dollar figure",
		blocking,
	)

	buyerSavings := BuyerSavings{
		Unavailable: ptrUnavailable(unavailable(
			"buyer_savings",
			"buyer savings is a counterfactual (external price − Merc price); "+
				"no bound external list price or measured alternative invoice exists in-repo; "+
				"deriving savings from an unvalidated catalogue list price is refused",
			[]string{"counterfactual_external_price"},
		)),
	}

	// Supplier contribution from money-conservation path: ledger supplier credit.
	// Per-delivery on the physical execution only (not diluted across reuse).
	supplier := SupplierContribution{
		Available: ptrKnown(knownQty(
			float64(boundMoneySupplierPayableNanosTotal),
			"nanos_supplier_payable_total_over_cluster",
			boundMoneyConservationPath+"#cluster.supplier_payable_nanos_total",
			fmt.Sprintf(
				"exact supplier ledger credit for %d deliveries / %d physical execution(s); "+
					"money conservation: buyer charge = supplier credit + platform row; "+
					"not the supplier's physical cost stack",
				boundMoneyDeliveries, boundMoneyPhysicalExecutions,
			),
			CategoryKnown,
		)),
		PhysicalCostOfSupplier: unavailable(
			"supplier_physical_cost",
			"Merc credits catalogue entitlement; supplier energy, depreciation, and residency are the supplier's cost and are not metered into Merc's books on community/owned supply",
			[]string{dirCatEnergy, dirCatStartupResidency, "depreciation"},
		),
	}

	gross := &GrossPlatformLedgerRow{
		Label:    "gross_platform_ledger_row_not_true_net",
		Nanos:    boundMoneyKnownCostContributionNanosTotal,
		Currency: boundMoneyCurrency,
		Source:   boundMoneyConservationPath + "#cluster.known_cost_contribution_nanos_total",
		Basis: "buyer_charge_nanos_total − supplier_payable_nanos_total on the 128→1 reuse " +
			"money path; equals KnownCostContributionNanos / ledger platform residual. " +
			"Processor, storage, egress, energy, and refund risk are not fully attributed " +
			"on this path — this is gross contribution, never true net.",
		NotTrueNet: "anti-gaming law: a gross platform ledger row must never be presented as Merc true net contribution",
	}

	trueNet := MercTrueNetContribution{
		Unavailable: ptrUnavailable(unavailable(
			"merc_true_net_contribution",
			"true net contribution remains structurally unavailable while any named directive cost category is UNKNOWN or ASSUMED rather than being presented as profit",
			blocking,
		)),
	}

	primary := buildPrimaryMetric(cats, joules)

	return Phase6EconomicsReceipt{
		SchemaVersion:               1,
		Kind:                        "phase6_directive_economics",
		Title:                       "Phase 6 physical economics: computed terms, structural refusals",
		Categories:                  cats,
		JoulesPerVerifiedOutcome:    &joules,
		CostPerVerifiedOutcome:      &fullCostBlocked,
		EnergyUSDPerVerifiedOutcome: &energyUSDQty,
		BuyerSavings:                buyerSavings,
		SupplierContribution:        supplier,
		MercTrueNet:                 trueNet,
		GrossPlatformRow:            gross,
		PrimaryMetric:               primary,
		MercTrueNetComputableToday:  false,
		PrimaryMetricQuotableToday:  false,
		HonestSummary: "Merc true net contribution is not computable today. " +
			"Joules per verified outcome are BOUND for GPU-domain Metal (0.1217 J). " +
			"Full cost per verified outcome is blocked by unknown categories. " +
			"Buyer savings has no counterfactual price. " +
			"Supplier contribution is known only as ledger entitlement on the money path. " +
			"The primary metric (outcomes per dollar per joule inside SLA) is not quotable: " +
			"dollar cost and SLA are incomplete. Gross platform ledger rows exist and must not be labeled true net.",
		Scope: Phase6Scope{
			Placement:      "local Apple M3 Ultra / llama.cpp Metal (owned supply); money path from public reuse conservation receipt",
			EnergyBoundary: "IOReport Energy Model channel 'GPU Energy' (AGX on-chip GPU domain only)",
			EnergyExcludes: []string{
				"CPU cores/clusters", "DRAM", "ANE", "display", "package total",
				"wall AC draw", "PSU inefficiency", "CUDA / NVIDIA (no device on this host)",
			},
			NoCUDAInvented:    true,
			GrossIsNotTrueNet: true,
			PricingDecisionTrueNet: "PricingDecision.FixedPoint.TrueNetContributionNanos is historical accepted-forecast " +
				"data, not settlement authority; only FINAL ContributionSettlement may publish batch-job true net, " +
				"and even that is not the Phase 6 directive physical metric while directive categories remain UNKNOWN",
		},
	}
}

func metalLocalCategoryInventory() []DirectiveCostCategory {
	return []DirectiveCostCategory{
		{
			Name:      dirCatActualDuration,
			Knowledge: CategoryKnown,
			Source:    boundEnergyAuthorityPath + "#load_window.window_s / workload.verified_outcomes",
			Notes: fmt.Sprintf(
				"measured wall duration of the load window: %.4fs for %d verified outcomes (%.6f ms/outcome); "+
					"includes model load / Metal graph compile (cold-start dilution named in energy receipt error_notes)",
				boundEnergyLoadWindowSeconds, boundEnergyVerifiedOutcomes,
				boundEnergyLoadWindowSeconds*1000.0/float64(boundEnergyVerifiedOutcomes),
			),
			NonMoney:     boundEnergyLoadWindowSeconds / float64(boundEnergyVerifiedOutcomes),
			NonMoneyUnit: "seconds_per_verified_outcome",
		},
		{
			Name:         dirCatUtilization,
			Knowledge:    CategoryUnknown,
			WouldRequire: "a production utilization signal on MeasuredSupplierLiabilityProxy (busy fraction of the placement over the billable window), not a default of 1.0",
			Notes:        "absent today — no production utilization meter on MeasuredSupplierLiabilityProxy or the energy harness",
		},
		{
			Name:      dirCatProviderSupplierRate,
			Knowledge: CategoryNotApplicable,
			Source:    "control/provider_cost_authority.go (owned/community supply)",
			Notes: "cloud provider hourly rate is not applicable on owned Metal. " +
				"Supplier catalogue entitlement is a separate money-path term (see supplier_contribution); " +
				"it is not a cloud provider rate. No CUDA provider rate is invented on this host.",
		},
		{
			Name:         dirCatStartupResidency,
			Knowledge:    CategoryUnknown,
			WouldRequire: "separate cold-start amortization (model load, graph compile, warm-resident replica $/hr allocation) measured and attributed per verified outcome",
			Notes:        "energy load window includes cold start but no startup/residency money model is attributed",
		},
		{
			Name:         dirCatVerification,
			Knowledge:    CategoryUnknown,
			WouldRequire: "Merc verification_outcome rollup (samples/fails) for the same unit of work; energy harness uses n_predict completion tokens as the outcome unit, not job verification passes",
			Notes:        "token count is not verification pass-rate; cannot scale cost by samples/passes on this path",
		},
		{
			Name:         dirCatRetries,
			Knowledge:    CategoryUnknown,
			WouldRequire: "task.retry_count rollup on the same workload (RetryRate on MeasuredSupplierLiabilityProxy)",
			Notes:        "energy harness is a single llama-cli invocation; no retry observations",
		},
		{
			Name:         dirCatStorageEgress,
			Knowledge:    CategoryUnknown,
			WouldRequire: "accepted/actual storage and egress bytes bound into a PricingDecision CostSchedule model for this unit of work",
			Notes: "CostSchedule publishes defaulted AWS rates with provenance (control/cost_schedule.go) " +
				"but this Metal energy path has no attributed bytes; rates without bytes are not a cost",
		},
		{
			Name:         dirCatPaymentAllocation,
			Knowledge:    CategoryUnknown,
			WouldRequire: "processor fee allocation from EconomicSchedule against a real buyer charge for this outcome",
			Notes:        "energy path has no buyer charge; money-path processor fee is modeled only when settlement runs",
		},
		{
			Name:      dirCatEnergy,
			Knowledge: CategoryKnown,
			Source:    boundEnergyAuthorityPath + "#joules_per_verified_outcome",
			Notes: fmt.Sprintf(
				"BOUND GPU-domain energy: %.9f J/verified_outcome (IOReport); "+
					"dollar conversion uses defaulted $%.2f/kWh (control/pricing.go) and is partial only",
				boundJoulesPerVerifiedOutcome, defaultElectricityUSDPerKWh,
			),
			NonMoney:     boundJoulesPerVerifiedOutcome,
			NonMoneyUnit: "joules_per_verified_outcome",
		},
		{
			Name:         dirCatRefundRisk,
			Knowledge:    CategoryUnknown,
			WouldRequire: "measured refund/dispute cohort rate for the product surface, or an accrued risk-reserve settlement tied to this outcome with release/consume ledger proof",
			Notes: "CostSchedule risk reserve is a 50 bps policy model (defaulted), not a measured loss rate; " +
				"policy alone does not make refund risk KNOWN for Phase 6 physical cost",
		},
	}
}

func unknownOrAssumedNames(cats []DirectiveCostCategory) []string {
	var out []string
	for _, c := range cats {
		switch c.Knowledge {
		case CategoryUnknown, CategoryAssumed:
			out = append(out, c.Name)
		}
	}
	return out
}

func buildPrimaryMetric(cats []DirectiveCostCategory, joules KnownQuantity) PrimarySystemMetric {
	blocking := unknownOrAssumedNames(cats)
	// Dollar cost for the primary metric means FULL cost per verified outcome,
	// not the partial energy-only figure.
	terms := PrimaryMetricTerms{
		VerifiedOutcomes: fmt.Sprintf("KNOWN count on energy path (%d completion_tokens) and money path (%d deliveries); unit differs by path",
			boundEnergyVerifiedOutcomes, boundMoneyDeliveries),
		DollarCost: "UNAVAILABLE — full cost_per_verified_outcome blocked by unknown directive categories " +
			"(partial energy-only USD must not substitute)",
		Joules: fmt.Sprintf("KNOWN GPU-domain %.9f J/outcome from %s",
			joules.Value, boundEnergyAuthorityPath),
		InsideSLA: "UNAVAILABLE — no SLA bound (MaxDurationMsPerUnit / contract SLA) evaluated on the energy harness",
	}
	// Primary metric factors that block quotability.
	factorBlocks := append([]string{}, blocking...)
	factorBlocks = append(factorBlocks, "full_dollar_cost_per_verified_outcome", "inside_sla")
	return PrimarySystemMetric{
		Name: primaryMetricName,
		Unavailable: ptrUnavailable(unavailable(
			primaryMetricName,
			"primary metric requires verified outcomes, full dollar cost per outcome, joules per outcome, and inside-SLA; "+
				"dollar cost and SLA are incomplete today — refusing to quote a number that drops those terms",
			factorBlocks,
		)),
		TermStatus: terms,
	}
}

func ptrUnavailable(u UnavailableQuantity) *UnavailableQuantity { return &u }
func ptrKnown(k KnownQuantity) *KnownQuantity                   { return &k }

// ValidatePhase6Receipt enforces structural honesty: true net and primary
// metric must not carry numbers while categories are incomplete; gross must
// declare it is not true net; joules must match the bound pin when present.
func ValidatePhase6Receipt(r Phase6EconomicsReceipt) error {
	if r.Kind != "phase6_directive_economics" {
		return fmt.Errorf("kind %q want phase6_directive_economics", r.Kind)
	}
	if len(r.Categories) == 0 {
		return fmt.Errorf("categories inventory is empty")
	}
	// Every directive name must appear exactly once.
	want := []string{
		dirCatActualDuration, dirCatUtilization, dirCatProviderSupplierRate,
		dirCatStartupResidency, dirCatVerification, dirCatRetries,
		dirCatStorageEgress, dirCatPaymentAllocation, dirCatEnergy, dirCatRefundRisk,
	}
	seen := map[string]int{}
	for _, c := range r.Categories {
		seen[c.Name]++
		if c.Knowledge == "" {
			return fmt.Errorf("category %q missing knowledge", c.Name)
		}
		if c.Knowledge == CategoryKnown && strings.TrimSpace(c.Source) == "" {
			return fmt.Errorf("category %q is KNOWN without source", c.Name)
		}
		if c.Knowledge == CategoryUnknown && strings.TrimSpace(c.WouldRequire) == "" {
			return fmt.Errorf("category %q is UNKNOWN without would_require", c.Name)
		}
	}
	for _, name := range want {
		if seen[name] != 1 {
			return fmt.Errorf("category %q appears %d times, want 1", name, seen[name])
		}
	}

	if r.MercTrueNet.IsAvailable() {
		return fmt.Errorf("true net marked available while programme says it is not yet computable")
	}
	if r.MercTrueNet.Unavailable == nil || r.MercTrueNet.Unavailable.Status != "unavailable" {
		return fmt.Errorf("true net must be structurally unavailable")
	}
	if r.MercTrueNet.Available != nil {
		return fmt.Errorf("true net must not carry an available number while blocked")
	}
	if r.MercTrueNetComputableToday {
		return fmt.Errorf("merc_true_net_computable_today must be false")
	}

	if r.PrimaryMetric.Available != nil || r.PrimaryMetricQuotableToday {
		return fmt.Errorf("primary metric must not be quotable today")
	}
	if r.PrimaryMetric.Unavailable == nil || r.PrimaryMetric.Unavailable.Status != "unavailable" {
		return fmt.Errorf("primary metric must be structurally unavailable")
	}
	if r.PrimaryMetric.Name != primaryMetricName {
		return fmt.Errorf("primary metric name %q", r.PrimaryMetric.Name)
	}

	if r.CostPerVerifiedOutcome == nil || r.CostPerVerifiedOutcome.Status != "unavailable" {
		return fmt.Errorf("full cost_per_verified_outcome must be unavailable")
	}
	if r.JoulesPerVerifiedOutcome == nil {
		return fmt.Errorf("joules_per_verified_outcome must be known")
	}
	if math.Abs(r.JoulesPerVerifiedOutcome.Value-boundJoulesPerVerifiedOutcome) > 1e-12 {
		return fmt.Errorf("joules pin mismatch: got %v want %v",
			r.JoulesPerVerifiedOutcome.Value, boundJoulesPerVerifiedOutcome)
	}
	if r.BuyerSavings.Available != nil || r.BuyerSavings.Unavailable == nil {
		return fmt.Errorf("buyer savings must be unavailable without a counterfactual price")
	}
	if r.GrossPlatformRow != nil {
		if r.GrossPlatformRow.Label != "gross_platform_ledger_row_not_true_net" {
			return fmt.Errorf("gross row label must refuse true-net aliasing")
		}
		if !strings.Contains(strings.ToLower(r.GrossPlatformRow.NotTrueNet), "never") {
			return fmt.Errorf("gross row must explicitly say it is never true net")
		}
	}
	if !r.Scope.NoCUDAInvented {
		return fmt.Errorf("scope must refuse invented CUDA cost")
	}
	if !r.Scope.GrossIsNotTrueNet {
		return fmt.Errorf("scope must mark gross ≠ true net")
	}
	return nil
}

// LoadBoundMetalEnergyAuthority reads the BOUND energy receipt and checks the
// joules pin. Returns error if missing, unbound, or drifted.
func LoadBoundMetalEnergyAuthority(repoRoot string) (joules float64, outcomes int, err error) {
	path := filepath.Join(repoRoot, boundEnergyAuthorityPath)
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, 0, err
	}
	var doc struct {
		BindingStatus            string  `json:"binding_status"`
		JoulesPerVerifiedOutcome float64 `json:"joules_per_verified_outcome"`
		Workload                 struct {
			VerifiedOutcomes int `json:"verified_outcomes"`
		} `json:"workload"`
		Measurement struct {
			Boundary string `json:"boundary"`
		} `json:"measurement"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return 0, 0, err
	}
	if strings.ToUpper(strings.TrimSpace(doc.BindingStatus)) != BindingBound {
		return 0, 0, fmt.Errorf("%s binding_status=%q want BOUND", boundEnergyAuthorityPath, doc.BindingStatus)
	}
	if math.Abs(doc.JoulesPerVerifiedOutcome-boundJoulesPerVerifiedOutcome) > 1e-12 {
		return 0, 0, fmt.Errorf("energy authority joules drifted: file=%v pin=%v",
			doc.JoulesPerVerifiedOutcome, boundJoulesPerVerifiedOutcome)
	}
	if doc.Workload.VerifiedOutcomes != boundEnergyVerifiedOutcomes {
		return 0, 0, fmt.Errorf("verified_outcomes drifted: file=%d pin=%d",
			doc.Workload.VerifiedOutcomes, boundEnergyVerifiedOutcomes)
	}
	if !strings.Contains(doc.Measurement.Boundary, "GPU") {
		return 0, 0, fmt.Errorf("energy boundary does not name GPU domain: %q", doc.Measurement.Boundary)
	}
	return doc.JoulesPerVerifiedOutcome, doc.Workload.VerifiedOutcomes, nil
}

// Phase6ReceiptJSON returns the receipt as JSON bytes (no producer_identity;
// binding is applied by the evidence writer).
func Phase6ReceiptJSON(r Phase6EconomicsReceipt) ([]byte, error) {
	if err := ValidatePhase6Receipt(r); err != nil {
		return nil, err
	}
	return json.MarshalIndent(r, "", "  ")
}
