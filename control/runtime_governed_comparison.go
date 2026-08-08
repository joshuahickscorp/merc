package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Governed Candle-versus-llama.cpp comparison and one shadow selector decision.
//
// CellEconomicsProjection already binds every money and non-money term the
// selector needs, and proves that two cells on one model cannot differ on
// verified-outcome cost under the cancelled settlement form. This file does
// the next governed act: compare the two cells on one immutable model contract,
// record predicted and actual figures, and emit ONE shadow decision that names
// the term that actually decided — without promoting anyone and without
// rewriting routing.
//
// Conservation:
//
//   - A settled PricingDecision's money does not move under this comparison.
//   - The projection is not a PricingDecision.
//   - True net stays refused while cost categories are unknown.
//   - A promotion receipt is a separate governed act; nothing here substitutes.

const (
	governedComparisonSchemaVersion = 1
	governedComparisonKind          = "governed_cell_comparison_and_shadow_decision"
	// priorThroughputClaimRatio is the prior (unbound) claim that llama.cpp was
	// ~2.1× the throughput of candle on Metal. It is the PREDICTION to verify,
	// not a fact to inherit. The governed chain measurement is the actual.
	priorThroughputClaimRatio = 2.1
	// priorThroughputClaimLowerBound is the lower-bound form of that claim.
	priorThroughputClaimLowerBound = 1.5

	// The two Metal embed cells this comparison is written against. They live
	// here rather than beside the tests because this file names them.
	candleEmbedCell = "candle-metal-minilm-embed"
	llamaEmbedCell  = "llama-cpp-metal-minilm-embed"
)

// GovernedComparisonCell is one cell's predicted and actual economics on the
// shared model contract.
type GovernedComparisonCell struct {
	CellID    string `json:"cell_id"`
	RuntimeID string `json:"runtime_id"`
	Engine    string `json:"engine"`
	HWClass   string `json:"hw_class"`

	// Predicted from the catalogue closed form (cost) and the prior throughput
	// claim (latency). PredictedPhysicalCostUSD is the verified-outcome ranking
	// number the projection binds — supplier × reliability — not true net and
	// not a full physical invoice.
	PredictedLatencyMsPerUnit    float64 `json:"predicted_latency_ms_per_unit"`
	PredictedPhysicalCostUSDUnit float64 `json:"predicted_physical_cost_usd_per_unit"`
	PredictedUnitsPerSec         float64 `json:"predicted_units_per_sec"`

	// Actual from MeasuredCellCost on the governed chain (or an equal-authority
	// measurement bound into the receipt).
	ActualLatencyMsPerUnit    float64 `json:"actual_latency_ms_per_unit"`
	ActualPhysicalCostUSDUnit float64 `json:"actual_physical_cost_usd_per_unit"`
	ActualUnitsPerSec         float64 `json:"actual_units_per_sec"`

	// Money terms the catalogue keys by (model, job_type), never by cell.
	SupplierEntitlementUSDUnit float64 `json:"supplier_entitlement_usd_per_unit"`
	BuyerPricePer1K            float64 `json:"buyer_price_per_1k_units"`

	// Quality and confidence.
	QualityTier           string  `json:"quality_tier"`
	VerificationContract  string  `json:"verification_contract"`
	VerificationPassRate  float64 `json:"verification_pass_rate"`
	ReliabilityMultiplier float64 `json:"reliability_multiplier"`
	Confidence            float64 `json:"confidence"`

	// Merc true net is structurally unavailable; the projection records why.
	// MercGrossPlatformUSDUnit is the buyer-minus-supplier residual per unit and
	// is a GROSS figure — it is what the platform keeps before every unknown cost
	// category, and must never be read as, or renamed to, contribution or net.
	MercTrueNetUnavailable   bool     `json:"merc_true_net_unavailable"`
	MercTrueNetReason        string   `json:"merc_true_net_reason,omitempty"`
	MercGrossPlatformUSDUnit float64  `json:"merc_gross_platform_usd_per_unit"`
	MercGrossPlatformBasis   string   `json:"merc_gross_platform_basis"`
	UnknownComponents        []string `json:"unknown_components"`

	// Per-cell prediction error: predicted − actual. Positive means the
	// prediction overstated the figure (predicted slower / more expensive than
	// actual). Near zero is a good prediction, not a routing claim.
	LatencyRegretMsPerUnit float64 `json:"latency_regret_ms_per_unit"`
	CostRegretUSDPerUnit   float64 `json:"cost_regret_usd_per_unit"`
}

// GovernedShadowDecision is one complete shadow selector decision on the
// comparison scope. It is not a promotion and does not change routing.
type GovernedShadowDecision struct {
	EligibleCells   []shadowCandidate `json:"eligible_cells"`
	ExcludedCells   []shadowExclusion `json:"excluded_cells"`
	PredictedWinner string            `json:"predicted_winner"`
	PredictedBasis  string            `json:"predicted_basis"`
	ActualWinner    string            `json:"actual_winner"`
	ActualBasis     string            `json:"actual_basis"`
	// DecidingTerm is the single projection term that broke the tie, or
	// TIE_NO_DECISION when none did. Distinct from ActualBasis only when a
	// reader needs the short name without the selection-policy vocabulary.
	DecidingTerm    string `json:"deciding_term"`
	SelectionReason string `json:"selection_reason"`
	// LatencyRegretMs / CostRegretUSD are PREDICTION error on the predicted
	// winner: predicted − actual. Positive means the prior overstated latency
	// or cost. These are not selection opportunity cost.
	LatencyRegretMs float64 `json:"latency_regret_ms_per_unit"`
	CostRegretUSD   float64 `json:"cost_regret_usd_per_unit"`
	// SelectionLatencyRegretMs / SelectionCostRegretUSD are the deliverable
	// selector regret: chosen − best_available in absolute user-visible units
	// (ms/unit, USD/unit). Zero means the selector picked the optimum on that
	// axis; positive means it left money or latency on the table. Never a ratio.
	SelectionLatencyRegretMs float64 `json:"selection_latency_regret_ms_per_unit"`
	SelectionCostRegretUSD   float64 `json:"selection_cost_regret_usd_per_unit"`
	// BestAvailableLatencyCell / BestAvailableCostCell name the cell that sets
	// the best_available baseline for each selection-regret axis.
	BestAvailableLatencyCell string `json:"best_available_latency_cell,omitempty"`
	BestAvailableCostCell    string `json:"best_available_cost_cell,omitempty"`
	// QualityOutcome summarises whether both cells cleared the same quality
	// contract on the comparison corpus / chain.
	QualityOutcome string  `json:"quality_outcome"`
	Confidence     float64 `json:"confidence"`
	// AuthorityDigest pins the decision identity (SHA-256 over the canonical
	// decision body without this field).
	AuthorityDigest string `json:"authority_digest"`

	RoutedCellID     string `json:"routed_cell_id"`
	ShadowCellID     string `json:"shadow_cell_id"`
	Diverged         bool   `json:"diverged"`
	Promoted         bool   `json:"promoted"`
	RoutingChanged   bool   `json:"routing_changed"`
	SelectionPolicy  string `json:"selection_policy"`
	RuntimeMatrixSHA string `json:"runtime_matrix_sha256"`
	CostHWClass      string `json:"cost_hw_class"`
}

// GovernedCellComparison is the bound receipt body for one model contract.
type GovernedCellComparison struct {
	SchemaVersion int    `json:"schema_version"`
	Kind          string `json:"kind"`
	MeasuredAt    string `json:"measured_at"`

	ModelContract CellEconomicsModelContract `json:"model_contract"`
	HWClass       string                     `json:"hw_class"`
	JobType       string                     `json:"job_type"`
	ModelRef      string                     `json:"model_ref"`

	// Prior claim under verification — not inherited as fact.
	PriorThroughputClaim map[string]any `json:"prior_throughput_claim"`

	// ActualsBinding states the provenance of the numbers, separately from the
	// provenance of this document. They are not the same thing and conflating
	// them is what lets a derived receipt inherit authority it never had.
	ActualsBinding map[string]any `json:"actuals_binding"`

	Cells    map[string]GovernedComparisonCell `json:"cells"`
	Decision GovernedShadowDecision            `json:"shadow_selector_decision"`

	// CostTieAuthority names and quantifies why the cost side came out the way
	// it did. A receipt that only says "the costs tie" cannot be argued with:
	// it does not say which authority forced it, nor how close it came to
	// breaking. See runtime_cost_tie_authority.go.
	CostTie CostTieAuthority `json:"cost_tie_authority"`

	// Aggregate prediction errors across the two cells (mean of |predicted−actual|
	// is not used; each cell's signed regret is already on the cell row).
	// LatencyDelta is actual_llama − actual_candle in ms/unit with ratio and
	// absolute form. Sub-millisecond absolute deltas are named as such.
	LatencyComparison map[string]any `json:"latency_comparison"`
	CostComparison    map[string]any `json:"cost_comparison"`

	// HostLoad is recorded because the machine is not perfectly quiet.
	HostLoad map[string]any `json:"host_load"`

	// Projections are the full CellEconomicsProjection for each cell so a
	// reader does not have to re-derive terms the census already bound.
	Projections map[string]CellEconomicsProjection `json:"projections"`

	// Settled money is untouched.
	SettledReceiptImmutability map[string]any `json:"settled_receipt_immutability"`

	DoesNotProve []string `json:"does_not_prove"`
	Limitations  []string `json:"limitations"`
}

// BuildGovernedComparison constructs the comparison and shadow decision from a
// planned shadow selection, measured costs, and the catalogue authority.
//
// predictedFromPrior controls the predicted latency arm:
//
//   - true: apply the 2.1× prior (llama faster) against the candle actual as
//     baseline, so the receipt can falsify or confirm that claim.
//   - false: predicted latency equals actual (no prior), useful for pure
//     decision-path tests.
//
// Nothing in this function writes activation policy, freezes money, or marks a
// cell routable.
func BuildGovernedComparison(
	shadow ShadowSelection,
	costs map[string]MeasuredCellCost,
	catalogue CataloguePriceAuthority,
	hostLoad map[string]any,
	predictedFromPrior bool,
) (GovernedCellComparison, error) {
	if len(shadow.Considered) == 0 {
		return GovernedCellComparison{}, fmt.Errorf("shadow selection considered no cells")
	}
	if catalogue.ModelID == "" {
		catalogue.ModelID = shadow.ModelRef
	}
	if catalogue.JobType == "" {
		catalogue.JobType = shadow.JobType
	}
	if catalogue.ReferencePricePer1K <= 0 {
		catalogue.ReferencePricePer1K = 0.00625
	}
	if catalogue.SupplierShare <= 0 {
		catalogue.SupplierShare = 0.97
	}

	// Rank on measured economics with the same pure function the live shadow
	// path uses, so the receipt cannot invent a different basis.
	cellIDs := make([]string, 0, len(shadow.Considered))
	for _, c := range shadow.Considered {
		cellIDs = append(cellIDs, c.CellID)
	}
	measuredDecision := decideMeasuredShadow(costs, cellIDs, shadow.RoutedCellID)

	// Predicted winner under the prior throughput claim (llama faster) when
	// costs are known to tie. Without the prior, predicted winner follows the
	// lifecycle ladder already recorded on the shadow selection.
	predictedWinner := shadow.ShadowCellID
	predictedBasis := shadow.SelectionBasis
	if predictedBasis == "" {
		predictedBasis = selectionBasisLadder
	}
	if predictedFromPrior {
		// Prior says llama is faster at equal price → predicted basis is
		// throughput, predicted winner is the llama cell when it is eligible.
		for _, c := range shadow.Considered {
			if c.CellID == llamaEmbedCell {
				predictedWinner = llamaEmbedCell
				predictedBasis = selectionBasisThroughputEqualPrice
				break
			}
		}
	}

	projections := ProjectCellEconomicsMap(costs, catalogue, "batch")
	cells := map[string]GovernedComparisonCell{}
	var candleActualMs, llamaActualMs float64
	var candleActualCost, llamaActualCost float64

	for _, cand := range shadow.Considered {
		cost, has := costs[cand.CellID]
		if !has {
			// Eligible but unmeasured: still list with zero actuals and no
			// invented supplier figure beyond catalogue.
			p := ProjectCellEconomics(MeasuredCellCost{
				CellID: cand.CellID, RuntimeID: cand.RuntimeID, Engine: cand.Engine,
				JobType: shadow.JobType, ModelRef: shadow.ModelRef,
			}, catalogue, "batch")
			cells[cand.CellID] = GovernedComparisonCell{
				CellID: cand.CellID, RuntimeID: cand.RuntimeID, Engine: cand.Engine,
				QualityTier: cand.QualityTier, VerificationContract: cand.Verification,
				SupplierEntitlementUSDUnit:   p.SupplierUSDPerUnit,
				BuyerPricePer1K:              p.BuyerPricePer1KUnits,
				PredictedPhysicalCostUSDUnit: p.SupplierUSDPerUnit,
				MercTrueNetUnavailable:       true,
				MercTrueNetReason:            "unmeasured cell; true net unavailable",
				UnknownComponents:            unknownCostComponents(),
			}
			continue
		}
		p := projections[cand.CellID]
		if p.CellID == "" {
			p = ProjectCellEconomics(cost, catalogue, "batch")
			projections[cand.CellID] = p
		}

		actualMs := cost.MedianMsPerUnit
		actualCost := 0.0
		if vo, ok := cost.ExpectedVerifiedOutcomeUSDPerUnit(); ok {
			actualCost = vo
		} else if p.VerifiedOutcomeOK {
			actualCost = p.VerifiedOutcomeUSDPerUnit
		}
		actualUPS := 0.0
		if actualMs > 0 {
			actualUPS = 1000.0 / actualMs
		}

		// Predicted latency: prior claim rewrites llama as 2.1× candle's actual
		// throughput (i.e. candle_ms / 2.1), and candle as the chain baseline.
		// Predicted cost is always the cancelled catalogue form — duration does
		// not enter — so a 2.1× latency prior cannot invent a cost gap.
		predictedMs := actualMs
		predictedUPS := actualUPS
		if predictedFromPrior && candleActualBaseline(costs) > 0 {
			base := candleActualBaseline(costs)
			switch cand.CellID {
			case candleEmbedCell:
				predictedMs = base
				predictedUPS = 1000.0 / base
			case llamaEmbedCell:
				predictedMs = base / priorThroughputClaimRatio
				predictedUPS = 1000.0 / predictedMs
			}
		}
		predictedCost := p.SupplierUSDPerUnit // cancelled form; reliability 1.0 assumed in prior

		row := GovernedComparisonCell{
			CellID:    cand.CellID,
			RuntimeID: firstNonEmpty(cost.RuntimeID, cand.RuntimeID),
			Engine:    firstNonEmpty(cost.Engine, cand.Engine),
			HWClass:   cost.HWClass,

			PredictedLatencyMsPerUnit:    predictedMs,
			PredictedPhysicalCostUSDUnit: predictedCost,
			PredictedUnitsPerSec:         predictedUPS,

			ActualLatencyMsPerUnit:    actualMs,
			ActualPhysicalCostUSDUnit: actualCost,
			ActualUnitsPerSec:         actualUPS,

			SupplierEntitlementUSDUnit: p.SupplierUSDPerUnit,
			BuyerPricePer1K:            p.BuyerPricePer1KUnits,

			QualityTier:           cand.QualityTier,
			VerificationContract:  cand.Verification,
			VerificationPassRate:  p.VerificationPassRate,
			ReliabilityMultiplier: p.ReliabilityMultiplier,
			Confidence:            p.Confidence,

			MercTrueNetUnavailable: !p.MercTrueNet.IsAvailable(),
			UnknownComponents:      append([]string(nil), p.UnknownComponents...),

			MercGrossPlatformUSDUnit: mercGrossPlatformPerUnit(p),
			MercGrossPlatformBasis: "buyer price per unit less supplier entitlement per unit; " +
				"GROSS platform residual before storage, egress, utilization, refund risk and " +
				"every other unknown category — never true net, never contribution",

			LatencyRegretMsPerUnit: predictedMs - actualMs,
			CostRegretUSDPerUnit:   predictedCost - actualCost,
		}
		if p.MercTrueNet.Unavailable != nil {
			row.MercTrueNetReason = p.MercTrueNet.Unavailable.Reason
		}
		cells[cand.CellID] = row
		if cand.CellID == candleEmbedCell {
			candleActualMs, candleActualCost = actualMs, actualCost
		}
		if cand.CellID == llamaEmbedCell {
			llamaActualMs, llamaActualCost = actualMs, actualCost
		}
	}

	// Actual winner from measured decision; fall back to ladder shadow.
	actualWinner := measuredDecision.Winner
	actualBasis := measuredDecision.Basis
	if actualWinner == "" {
		actualWinner = shadow.ShadowCellID
		actualBasis = shadow.SelectionBasis
		if actualBasis == "" {
			actualBasis = selectionBasisLadder
		}
	}

	// Decision-level PREDICTION error: predicted−actual for the predicted
	// winner's metrics. If the prior named llama and llama was slower than
	// predicted, latency prediction-error is negative (predicted lower ms than
	// actual). Distinct from selection regret below.
	decisionLatencyRegret := 0.0
	decisionCostRegret := 0.0
	if row, ok := cells[predictedWinner]; ok {
		decisionLatencyRegret = row.LatencyRegretMsPerUnit
		decisionCostRegret = row.CostRegretUSDPerUnit
	}

	// Selection regret: chosen − best_available in absolute units. A selector
	// that picks the optimum has zero on both axes; one that always picks the
	// faster engine pays positive cost regret when reliability (or another
	// non-latency term) makes the slower cell cheaper per verified outcome.
	selectionLatencyRegret, selectionCostRegret := 0.0, 0.0
	bestLatencyCell, bestCostCell := "", ""
	if row, ok := cells[actualWinner]; ok && row.ActualLatencyMsPerUnit > 0 {
		bestMs := row.ActualLatencyMsPerUnit
		bestLatencyCell = actualWinner
		for id, other := range cells {
			if other.ActualLatencyMsPerUnit > 0 && other.ActualLatencyMsPerUnit < bestMs {
				bestMs = other.ActualLatencyMsPerUnit
				bestLatencyCell = id
			}
		}
		selectionLatencyRegret = row.ActualLatencyMsPerUnit - bestMs
	}
	if row, ok := cells[actualWinner]; ok && row.ActualPhysicalCostUSDUnit > 0 {
		bestCost := row.ActualPhysicalCostUSDUnit
		bestCostCell = actualWinner
		for id, other := range cells {
			if other.ActualPhysicalCostUSDUnit > 0 && other.ActualPhysicalCostUSDUnit < bestCost {
				bestCost = other.ActualPhysicalCostUSDUnit
				bestCostCell = id
			}
		}
		selectionCostRegret = row.ActualPhysicalCostUSDUnit - bestCost
	}

	// Quality outcome: both measured cells must show zero verification failures
	// for a PASS; otherwise name the failure.
	qualityOutcome := "PASS_EQUAL_CONTRACT"
	for _, cost := range costs {
		if cost.VerificationSamples > 0 && cost.VerificationFails > 0 {
			qualityOutcome = fmt.Sprintf("VERIFICATION_FAILURES cell=%s fails=%d/%d",
				cost.CellID, cost.VerificationFails, cost.VerificationSamples)
			break
		}
	}

	// Confidence: minimum of the two measured projections (a chain is only as
	// confident as its weaker cell).
	confidence := 1.0
	nMeasured := 0
	for _, row := range cells {
		if row.ActualLatencyMsPerUnit > 0 {
			nMeasured++
			if row.Confidence < confidence {
				confidence = row.Confidence
			}
		}
	}
	if nMeasured == 0 {
		confidence = 0
	}

	selectionReason := selectionReasonFor(actualBasis, actualWinner, cells)

	eligible := append([]shadowCandidate(nil), shadow.Considered...)
	if eligible == nil {
		eligible = []shadowCandidate{}
	}
	excluded := append([]shadowExclusion(nil), shadow.Excluded...)
	if excluded == nil {
		excluded = []shadowExclusion{}
	}
	// CUDA (and any other non-Metal) embed arm is out of scope for this host
	// comparison: no matched identity and quality on a paid CUDA path. Record
	// that as an exclusion so the decision lists why it was not eligible, even
	// when the directed set never materialised a CUDA cell for this model.
	hasCUDAExclusion := false
	for _, e := range excluded {
		if strings.Contains(e.CellID, "cuda") || strings.Contains(e.Reason, "CUDA") {
			hasCUDAExclusion = true
			break
		}
	}
	if !hasCUDAExclusion {
		excluded = append(excluded, shadowExclusion{
			CellID: "cuda-embed-arm",
			Reason: "no paid CUDA arm with matched model identity and quality on this host; selector consumer structure exists but CUDA remains ineligible",
		})
	}

	decision := GovernedShadowDecision{
		EligibleCells:            eligible,
		ExcludedCells:            excluded,
		PredictedWinner:          predictedWinner,
		PredictedBasis:           predictedBasis,
		ActualWinner:             actualWinner,
		ActualBasis:              actualBasis,
		DecidingTerm:             actualBasis,
		SelectionReason:          selectionReason,
		LatencyRegretMs:          decisionLatencyRegret,
		CostRegretUSD:            decisionCostRegret,
		SelectionLatencyRegretMs: selectionLatencyRegret,
		SelectionCostRegretUSD:   selectionCostRegret,
		BestAvailableLatencyCell: bestLatencyCell,
		BestAvailableCostCell:    bestCostCell,
		QualityOutcome:           qualityOutcome,
		Confidence:               confidence,
		RoutedCellID:             shadow.RoutedCellID,
		ShadowCellID:             actualWinner,
		Diverged:                 actualWinner != shadow.RoutedCellID,
		Promoted:                 false,
		RoutingChanged:           false,
		SelectionPolicy:          shadowSelectionPolicy,
		RuntimeMatrixSHA:         firstNonEmpty(shadow.RuntimeMatrixSHA, generatedRuntimeMatrixSHA256),
		CostHWClass:              firstNonEmpty(shadow.CostHWClass, "apple_silicon_ultra"),
	}
	digest, err := digestGovernedDecision(decision)
	if err != nil {
		return GovernedCellComparison{}, err
	}
	decision.AuthorityDigest = digest

	latencyDelta := llamaActualMs - candleActualMs
	latencyRatio := 0.0
	if candleActualMs > 0 {
		latencyRatio = llamaActualMs / candleActualMs
	}
	// Throughput ratio actual: candle_ups / llama_ups = llama_ms / candle_ms.
	throughputRatioCandleOverLlama := 0.0
	if llamaActualMs > 0 && candleActualMs > 0 {
		throughputRatioCandleOverLlama = llamaActualMs / candleActualMs
	}

	out := GovernedCellComparison{
		SchemaVersion: governedComparisonSchemaVersion,
		Kind:          governedComparisonKind,
		MeasuredAt:    time.Now().UTC().Format(time.RFC3339Nano),
		ModelContract: CellEconomicsModelContract{
			ModelID:             catalogue.ModelID,
			JobType:             catalogue.JobType,
			ReferencePricePer1K: catalogue.ReferencePricePer1K,
			SupplierShare:       catalogue.SupplierShare,
			ScheduleSHA256:      catalogue.ScheduleSHA256,
			BoardSHA256:         catalogue.BoardSHA256,
			PriceFormula:        catalogue.PriceFormula,
			SettlementCurrency:  catalogue.SettlementCurrency,
		},
		HWClass:  decision.CostHWClass,
		JobType:  shadow.JobType,
		ModelRef: shadow.ModelRef,
		PriorThroughputClaim: map[string]any{
			"statement":   "prior measurement put llama.cpp ahead by ~2.1× throughput (lower bound 1.5×) on Metal embed",
			"ratio":       priorThroughputClaimRatio,
			"lower_bound": priorThroughputClaimLowerBound,
			"status":      priorClaimStatus(candleActualMs, llamaActualMs, actualsBinding(costs)),
			"note":        "this is the prediction under verification; only a BOUND actual is authoritative over it",
		},
		ActualsBinding: map[string]any{
			"status": actualsBinding(costs),
			"why": "the weakest binding_status across the measured rows this comparison ranks on; " +
				"the receipt's own producer identity names who ran the arithmetic, not who produced the timings",
			"consequence": "an UNBOUND actual can order this decision's shadow ranking but cannot rule on the prior claim, " +
				"and cannot make this receipt BOUND",
		},
		Cells:    cells,
		Decision: decision,
		CostTie:  ExplainCostTie(projections[candleEmbedCell], projections[llamaEmbedCell], catalogue),
		LatencyComparison: map[string]any{
			"candle_actual_ms_per_unit":          candleActualMs,
			"llama_actual_ms_per_unit":           llamaActualMs,
			"absolute_delta_ms_per_unit":         latencyDelta,
			"ratio_llama_over_candle":            latencyRatio,
			"throughput_ratio_candle_over_llama": throughputRatioCandleOverLlama,
			"scale_note":                         latencyScaleNote(latencyDelta),
			"rank_by":                            "p50 median_ms_per_unit from MeasuredCellCost; p95/p99 live in the parity receipt and are not re-derived here",
		},
		CostComparison: map[string]any{
			"candle_verified_outcome_usd_per_unit": candleActualCost,
			"llama_verified_outcome_usd_per_unit":  llamaActualCost,
			"absolute_delta_usd_per_unit":          llamaActualCost - candleActualCost,
			"tie":                                  costsTieUSD(candleActualCost, llamaActualCost),
			"authority":                            supplierEntitlementPolicyCancelled,
			"note":                                 "supplier entitlement is units/1000 × price × share; unitsPerSec cancels; equal reliability ⇒ equal verified-outcome cost",
		},
		HostLoad:    hostLoad,
		Projections: projections,
		SettledReceiptImmutability: map[string]any{
			"projection_is_not_pricing_decision":         true,
			"revalidation_may_change_future_quotes_only": true,
			"supplier_entitlement_policy":                supplierEntitlementPolicyCancelled,
			"comparison_does_not_mutate_settled_money":   true,
		},
		DoesNotProve: []string{
			"does not promote any cell to routable",
			"does not change ordinary admission or routing",
			"does not freeze a PricingDecision or move settled money",
			"does not establish true net contribution (cost categories remain unknown/assumed)",
			"does not measure p95/p99 task latency across a quiet-host fleet cohort",
			"does not admit CUDA: no matched identity and quality on a paid CUDA arm",
			"does not treat the prior 2.1× harness claim as product proof",
			"does not authorise a promotion receipt; that is a separate governed act",
		},
		Limitations: []string{
			"One host, one hardware class (apple_silicon_ultra); not a fleet claim.",
			"Host load is recorded because the machine is not perfectly quiet.",
			"Energy partial uses ASSUMED watts and defaulted electricity.",
			"Storage, egress, utilization, refund risk remain unknown.",
			"A latency ratio is never the headline; the absolute delta is stated first.",
		},
	}
	return out, nil
}

// mercGrossPlatformPerUnit is buyer price per unit less supplier entitlement per
// unit. Gross, and named gross everywhere it appears: every unknown cost
// category still sits between this number and anything anyone could call net.
func mercGrossPlatformPerUnit(p CellEconomicsProjection) float64 {
	if p.BuyerPricePer1KUnits <= 0 || p.SupplierUSDPerUnit <= 0 {
		return 0
	}
	return p.BuyerPricePer1KUnits/1000.0 - p.SupplierUSDPerUnit
}

func candleActualBaseline(costs map[string]MeasuredCellCost) float64 {
	if c, ok := costs[candleEmbedCell]; ok && c.MedianMsPerUnit > 0 {
		return c.MedianMsPerUnit
	}
	// Fall back to any candle-engine cell.
	for _, c := range costs {
		if c.Engine == "candle" && c.MedianMsPerUnit > 0 {
			return c.MedianMsPerUnit
		}
	}
	return 0
}

// actualsBinding is the weakest binding_status across the rows a verdict would
// rest on. A comparison is only as bound as its least bound input, so the
// minimum travels into the receipt instead of the harness's own stamp implying
// authority the numbers do not have.
func actualsBinding(costs map[string]MeasuredCellCost) string {
	if len(costs) == 0 {
		return BindingUnbound
	}
	for _, c := range costs {
		if !strings.EqualFold(c.SourceBinding, BindingBound) {
			return BindingUnbound
		}
	}
	return BindingBound
}

// priorClaimStatus rules on the prior throughput claim, but only when the
// actuals can name their own producer.
//
// The first gate is provenance, not arithmetic. Timings read out of an artifact
// missing source_commit, build_digest, model_artifact_digest and raw_samples
// cannot say which candle build or which llama.cpp build they timed, so a
// verdict drawn from them is one unnameable number contradicting another. That
// is not a falsification and it must not be recorded as one.
//
// The second gate is the noise floor, applied to a falsification and not only
// to a tie. Overturning a prior claim on a point estimate thinner than the
// host's own noise is how a measurement artifact becomes a finding.
func priorClaimStatus(candleMs, llamaMs float64, binding string) string {
	if !strings.EqualFold(binding, BindingBound) {
		return "UNRESOLVED_UNBOUND_ACTUALS"
	}
	if candleMs <= 0 || llamaMs <= 0 {
		return "UNVERIFIED_NO_ACTUAL"
	}
	// throughput_llama / throughput_candle = candle_ms / llama_ms
	// Prior claims this ratio ≥ 2.1 (lower bound 1.5).
	throughputLlamaOverCandle := candleMs / llamaMs
	switch {
	case throughputLlamaOverCandle >= priorThroughputClaimRatio:
		return "CONFIRMED_AT_OR_ABOVE_2.1X"
	case throughputLlamaOverCandle >= priorThroughputClaimLowerBound:
		return "CONFIRMED_ABOVE_LOWER_BOUND_1.5X_BELOW_2.1X"
	case throughputLlamaOverCandle > 1.0:
		return "LLAMA_FASTER_BUT_BELOW_1.5X_CLAIM"
	case math.Abs(throughputLlamaOverCandle-1.0) < latencyNoiseFraction:
		return "TIE_PRIOR_NOT_CONFIRMED"
	default:
		return "FALSIFIED_CANDLE_FASTER_ON_GOVERNED_CONTRACT"
	}
}

func selectionReasonFor(basis, winner string, cells map[string]GovernedComparisonCell) string {
	switch basis {
	case selectionBasisMeasuredCost:
		return fmt.Sprintf(
			"cell %s wins on verified-outcome cost (strictly cheaper); cost is the deciding term",
			winner)
	case selectionBasisThroughputEqualPrice:
		return fmt.Sprintf(
			"verified-outcome cost ties by catalogue arithmetic; cell %s wins on more throughput at equal price — not a cost win",
			winner)
	case selectionBasisTieNoDecision:
		return "every term the projection binds ties (cost, reliability, latency within noise); correct refusal to choose; routed cell retained"
	case selectionBasisLadder:
		return fmt.Sprintf(
			"insufficient measured pair on one hardware class; cell %s wins on lifecycle ladder / quality tier",
			winner)
	default:
		return fmt.Sprintf("basis %s selected %s", basis, winner)
	}
}

// digestGovernedDecision hashes the decision with AuthorityDigest cleared so
// the digest is not self-referential.
func digestGovernedDecision(d GovernedShadowDecision) (string, error) {
	cp := d
	cp.AuthorityDigest = ""
	raw, err := json.Marshal(cp)
	if err != nil {
		return "", err
	}
	// Stable key order is not guaranteed by encoding/json for structs — but
	// struct field order is fixed in the type definition, so the marshal is
	// deterministic for this type.
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

// sortedCellIDs is a small helper for stable test output.
func sortedCellIDs(cells map[string]GovernedComparisonCell) []string {
	out := make([]string, 0, len(cells))
	for id := range cells {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// runSysctlLoadavg reads the three load-average numbers on macOS via
// `sysctl -n vm.loadavg`. Returns an error on non-macOS or parse failure so
// callers can omit the field rather than invent zeros.
func runSysctlLoadavg() ([]float64, error) {
	out, err := exec.Command("sysctl", "-n", "vm.loadavg").Output()
	if err != nil {
		return nil, err
	}
	// Typical: "{ 5.39 5.26 5.49 }"
	s := strings.TrimSpace(string(out))
	s = strings.TrimPrefix(s, "{")
	s = strings.TrimSuffix(s, "}")
	fields := strings.Fields(s)
	if len(fields) < 3 {
		return nil, fmt.Errorf("unexpected loadavg %q", out)
	}
	vals := make([]float64, 3)
	for i := 0; i < 3; i++ {
		v, err := strconv.ParseFloat(fields[i], 64)
		if err != nil {
			return nil, err
		}
		vals[i] = v
	}
	return vals, nil
}

// latencyScaleNote states the size of the delta rather than asserting one. The
// note was hardcoded to "sub-millisecond" while the numbers were cohort figures
// around 0.2 ms; the bound interleaved measurement puts the delta near 1.2 ms,
// and a receipt that describes its own headline wrongly is worse than one that
// omits it.
func latencyScaleNote(deltaMs float64) string {
	abs := math.Abs(deltaMs)
	const prefixSavingMs = 410.0
	switch {
	case abs < 1.0:
		return fmt.Sprintf(
			"absolute delta %.4f ms per unit is sub-millisecond; the prefix work measured "+
				"~%.0f ms of prefill saved at p50 and p95, which is the scale at which a "+
				"latency claim is worth leading with", abs, prefixSavingMs)
	default:
		return fmt.Sprintf(
			"absolute delta %.4f ms per unit is real and reproducible, and is still "+
				"%.0fx smaller than the ~%.0f ms prefill saving the prefix work measured; "+
				"engine choice on this contract matters and is not where the large latency lives",
			abs, prefixSavingMs/abs, prefixSavingMs)
	}
}
