package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
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
// selector needs, and proves that two equally reliable cells on one model cannot
// differ on supplier liability under the cancelled settlement form. This file does
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
	governedComparisonSchemaVersion = 2
	governedComparisonKind          = "governed_cell_comparison_and_shadow_decision"
	// priorThroughputClaimRatio is the prior (unbound) claim that llama.cpp was
	// ~2.1× the throughput of candle on Metal. It is the PREDICTION to verify,
	// not a fact to inherit. The governed chain measurement is the actual.
	priorThroughputClaimRatio = 2.1
	// priorThroughputClaimLowerBound is the lower-bound form of that claim.
	priorThroughputClaimLowerBound = 1.5
	governedLatencyMinSamples      = 20

	// The two Metal embed cells this comparison is written against. They live
	// here rather than beside the tests because this file names them.
	candleEmbedCell = "candle-metal-minilm-embed"
	llamaEmbedCell  = "llama-cpp-metal-minilm-embed"
)

// GovernedComparisonCell is one cell's predicted and actual economics on the
// shared model contract.
type GovernedComparisonCell struct {
	CellID           string `json:"cell_id"`
	RuntimeID        string `json:"runtime_id"`
	Engine           string `json:"engine"`
	HWClass          string `json:"hw_class"`
	HardwareIdentity string `json:"hardware_identity,omitempty"`
	// Currency governs every legacy `_usd` field on this row. Those names are
	// retained for receipt compatibility, not as an implicit USD conversion.
	Currency string `json:"currency"`

	// Predicted from the catalogue closed form (supplier liability) and the prior
	// throughput claim (latency). This is explicitly not platform cost.
	PredictedLatencyMsPerUnit                    float64 `json:"predicted_latency_ms_per_unit"`
	PredictedSupplierLiabilityUSDPerVerifiedUnit float64 `json:"predicted_supplier_liability_usd_per_verified_unit"`
	PredictedUnitsPerSec                         float64 `json:"predicted_units_per_sec"`

	// Actual from MeasuredSupplierLiabilityProxy on the governed chain (or an equal-authority
	// measurement bound into the receipt).
	ActualLatencyMsPerUnit                    float64 `json:"actual_latency_ms_per_unit"`
	ActualSupplierLiabilityUSDPerVerifiedUnit float64 `json:"actual_supplier_liability_usd_per_verified_unit"`
	ActualUnitsPerSec                         float64 `json:"actual_units_per_sec"`
	LatencySourceBinding                      string  `json:"latency_source_binding"`
	SupplierLiabilitySourceBinding            string  `json:"supplier_liability_source_binding"`

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
	// The legacy gross slot remains zero unless Available is true. This comparison
	// has no frozen mapping from settlement input units to accepted output records,
	// so it currently refuses gross instead of subtracting unlike denominators.
	MercTrueNetUnavailable     bool     `json:"merc_true_net_unavailable"`
	MercTrueNetReason          string   `json:"merc_true_net_reason,omitempty"`
	MercGrossPlatformAvailable bool     `json:"merc_gross_platform_available"`
	MercGrossPlatformUSDUnit   float64  `json:"merc_gross_platform_usd_per_unit"`
	MercGrossPlatformBasis     string   `json:"merc_gross_platform_basis"`
	UnknownComponents          []string `json:"unknown_components"`

	// Per-cell prediction error: predicted − actual. Positive means the
	// prediction overstated the figure (predicted slower / more expensive than
	// actual). Near zero is a good prediction, not a routing claim.
	LatencyRegretMsPerUnit                     float64 `json:"latency_regret_ms_per_unit"`
	SupplierLiabilityPredictionErrorUSDPerUnit float64 `json:"supplier_liability_prediction_error_usd_per_verified_unit"`
}

// GovernedLatencyActual is deliberately incapable of carrying money or
// reliability evidence. A benchmark that only embeds and times may populate
// this type; it cannot be converted into a MeasuredSupplierLiabilityProxy by
// inventing a catalogue payout, verification passes, retries, or terminal
// outcomes. BuildGovernedComparison uses these rows for latency diagnostics and
// prior-claim evaluation only. Shadow selection continues to consume the
// independently supplied supplier-liability rows.
type GovernedLatencyActual struct {
	CellID           string  `json:"cell_id"`
	RuntimeID        string  `json:"runtime_id"`
	Engine           string  `json:"engine"`
	HWClass          string  `json:"hw_class"`
	HardwareIdentity string  `json:"hardware_identity,omitempty"`
	Samples          int     `json:"samples"`
	Units            int64   `json:"units"`
	MedianMsPerUnit  float64 `json:"median_ms_per_unit"`
	SourceBinding    string  `json:"source_binding"`
}

// GovernedShadowDecision is one complete shadow selector decision on the
// comparison scope. It is not a promotion and does not change routing.
type GovernedShadowDecision struct {
	// Currency governs every legacy `_usd` prediction-error and regret field in
	// this independently digested decision object.
	Currency        string            `json:"currency"`
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
	LatencyRegretMs                     float64 `json:"latency_regret_ms_per_unit"`
	SupplierLiabilityPredictionErrorUSD float64 `json:"supplier_liability_prediction_error_usd_per_verified_unit"`
	// SelectionLatencyRegretMs / SelectionCostRegretUSD are the deliverable
	// selector regret: chosen − best_available in absolute user-visible units
	// (ms/unit, USD/unit). Zero means the selector picked the optimum on that
	// axis; positive means it left money or latency on the table. Never a ratio.
	SelectionLatencyRegretMs            float64 `json:"selection_latency_regret_ms_per_unit"`
	SelectionSupplierLiabilityRegretUSD float64 `json:"selection_supplier_liability_regret_usd_per_verified_unit"`
	// BestAvailableLatencyCell / BestAvailableCostCell name the cell that sets
	// the best_available baseline for each selection-regret axis.
	BestAvailableLatencyCell           string `json:"best_available_latency_cell,omitempty"`
	BestAvailableSupplierLiabilityCell string `json:"best_available_supplier_liability_cell,omitempty"`
	EconomicsRefusal                   string `json:"economics_refusal,omitempty"`
	// QualityOutcome summarises whether both cells cleared the same quality
	// contract on the comparison corpus / chain.
	QualityOutcome string  `json:"quality_outcome"`
	Confidence     float64 `json:"confidence"`
	// AuthorityDigest pins the decision identity (SHA-256 over the canonical
	// decision body without this field).
	AuthorityDigest string `json:"authority_digest"`

	RoutedCellID                      string `json:"routed_cell_id"`
	ShadowCellID                      string `json:"shadow_cell_id"`
	Diverged                          bool   `json:"diverged"`
	Promoted                          bool   `json:"promoted"`
	RoutingChanged                    bool   `json:"routing_changed"`
	SelectionPolicy                   string `json:"selection_policy"`
	RuntimeMatrixSHA                  string `json:"runtime_matrix_sha256"`
	SupplierLiabilityHWClass          string `json:"supplier_liability_hw_class"`
	SupplierLiabilityHardwareIdentity string `json:"supplier_liability_hardware_identity"`
}

// GovernedCellComparison is the bound receipt body for one model contract.
type GovernedCellComparison struct {
	SchemaVersion int    `json:"schema_version"`
	Kind          string `json:"kind"`
	MeasuredAt    string `json:"measured_at"`

	ModelContract    CellEconomicsModelContract `json:"model_contract"`
	HWClass          string                     `json:"hw_class"`
	HardwareIdentity string                     `json:"hardware_identity"`
	JobType          string                     `json:"job_type"`
	ModelRef         string                     `json:"model_ref"`

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
// planned shadow selection, measured supplier-liability proxies, an explicitly
// separate latency input, and the catalogue authority. The separation is an
// authority boundary: a latency-only benchmark may describe latency and rule on
// a latency prior, but never supplies payout, reliability, or selector inputs.
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
	costs map[string]MeasuredSupplierLiabilityProxy,
	latencies map[string]GovernedLatencyActual,
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
	if catalogue.ReferencePricePer1K <= 0 || math.IsNaN(catalogue.ReferencePricePer1K) ||
		math.IsInf(catalogue.ReferencePricePer1K, 0) {
		return GovernedCellComparison{}, fmt.Errorf(
			"governed comparison has no explicit positive catalogue reference price")
	}
	if catalogue.SupplierShare <= 0 || math.IsNaN(catalogue.SupplierShare) ||
		math.IsInf(catalogue.SupplierShare, 0) {
		return GovernedCellComparison{}, fmt.Errorf(
			"governed comparison has no explicit positive supplier share")
	}
	_, settlementCurrency, settlementPriceOK := cellEconomicsSettlementPrice(catalogue)
	if !settlementPriceOK {
		return GovernedCellComparison{}, fmt.Errorf(
			"governed comparison has no valid buyer price authority in its settlement currency")
	}
	if err := validateGovernedSupplierCurrencies(
		shadow, costs, catalogue, settlementCurrency); err != nil {
		return GovernedCellComparison{}, err
	}
	if err := validateGovernedLatencyActuals(shadow, costs, latencies); err != nil {
		return GovernedCellComparison{}, err
	}

	// Rank on measured economics with the same pure function the live shadow
	// path uses, so the receipt cannot invent a different basis.
	cellIDs := make([]string, 0, len(shadow.Considered))
	for _, c := range shadow.Considered {
		cellIDs = append(cellIDs, c.CellID)
	}
	measuredDecision := decideMeasuredSupplierLiabilityShadow(costs, cellIDs, shadow.RoutedCellID)
	supplierLiabilityHWClass, supplierLiabilityHardwareIdentity, err :=
		exactSupplierLiabilityHardwareScope(costs, cellIDs,
			shadow.SupplierLiabilityHWClass,
			shadow.SupplierLiabilityHardwareIdentity)
	if err != nil {
		return GovernedCellComparison{}, err
	}

	// Predicted winner under the prior throughput claim (llama faster) when
	// costs are known to tie. Without the prior, predicted winner follows the
	// lifecycle ladder already recorded on the shadow selection.
	predictedWinner := shadow.ShadowCellID
	predictedBasis := shadow.SelectionBasis
	if predictedBasis == "" {
		predictedBasis = selectionBasisLadder
	}
	if predictedFromPrior {
		// Prior says llama is faster at equal supplier liability → predicted basis is
		// throughput, predicted winner is the llama cell when it is eligible.
		for _, c := range shadow.Considered {
			if c.CellID == llamaEmbedCell {
				predictedWinner = llamaEmbedCell
				predictedBasis = selectionBasisThroughputEqualLiability
				break
			}
		}
	}

	projections := ProjectCellEconomicsMap(costs, catalogue, "batch")
	cells := map[string]GovernedComparisonCell{}
	var candleActualMs, llamaActualMs float64
	var candleActualCost, llamaActualCost float64

	for _, cand := range shadow.Considered {
		latency := latencies[cand.CellID]
		actualMs := latency.MedianMsPerUnit
		actualUPS := 1000.0 / actualMs
		predictedMs := actualMs
		predictedUPS := actualUPS
		if predictedFromPrior && candleActualBaseline(latencies) > 0 {
			base := candleActualBaseline(latencies)
			switch cand.CellID {
			case candleEmbedCell:
				predictedMs = base
				predictedUPS = 1000.0 / base
			case llamaEmbedCell:
				predictedMs = base / priorThroughputClaimRatio
				predictedUPS = 1000.0 / predictedMs
			}
		}

		cost, has := costs[cand.CellID]
		if !has {
			// Eligible but supplier-unmeasured: latency remains an independently
			// sourced actual, while payout/reliability and selection stay refused.
			p := ProjectCellEconomics(MeasuredSupplierLiabilityProxy{
				CellID: cand.CellID, RuntimeID: cand.RuntimeID, Engine: cand.Engine,
				JobType: shadow.JobType, ModelRef: shadow.ModelRef,
			}, catalogue, "batch")
			cells[cand.CellID] = GovernedComparisonCell{
				CellID: cand.CellID, RuntimeID: cand.RuntimeID, Engine: cand.Engine,
				HWClass: latency.HWClass, HardwareIdentity: latency.HardwareIdentity,
				Currency:    p.Currency,
				QualityTier: cand.QualityTier, VerificationContract: cand.Verification,
				SupplierEntitlementUSDUnit:                   p.SupplierUSDPerUnit,
				BuyerPricePer1K:                              p.BuyerPricePer1KUnits,
				PredictedSupplierLiabilityUSDPerVerifiedUnit: p.SupplierUSDPerUnit,
				PredictedLatencyMsPerUnit:                    predictedMs,
				PredictedUnitsPerSec:                         predictedUPS,
				ActualLatencyMsPerUnit:                       actualMs,
				ActualUnitsPerSec:                            actualUPS,
				LatencySourceBinding:                         normalizedEvidenceBinding(latency.SourceBinding),
				SupplierLiabilitySourceBinding:               BindingUnbound,
				LatencyRegretMsPerUnit:                       predictedMs - actualMs,
				MercTrueNetUnavailable:                       true,
				MercTrueNetReason:                            "unmeasured cell; true net unavailable",
				MercGrossPlatformAvailable:                   false,
				MercGrossPlatformBasis:                       governedGrossPlatformRefusal,
				UnknownComponents:                            unknownPlatformCostComponents(),
			}
			if cand.CellID == candleEmbedCell {
				candleActualMs = actualMs
			}
			if cand.CellID == llamaEmbedCell {
				llamaActualMs = actualMs
			}
			continue
		}
		p := projections[cand.CellID]
		if p.CellID == "" {
			p = ProjectCellEconomics(cost, catalogue, "batch")
			projections[cand.CellID] = p
		}

		// ProjectCellEconomics has already enforced that the measured payout and
		// catalogue share one settlement currency. Keep the truthful payable value
		// when reliability refuses selection, but never reintroduce a raw amount
		// that the projection rejected for belonging to another currency.
		actualCost := p.SupplierUSDPerUnit

		// Predicted latency: prior claim rewrites llama as 2.1× candle's actual
		// throughput (i.e. candle_ms / 2.1), and candle as the chain baseline.
		// Predicted cost is always the cancelled catalogue form — duration does
		// not enter — so a 2.1× latency prior cannot invent a cost gap.
		predictedCost := p.SupplierUSDPerUnit // cancelled form; reliability 1.0 assumed in prior

		gross, grossAvailable := mercGrossPlatformPerUnit(p)
		row := GovernedComparisonCell{
			CellID:           cand.CellID,
			RuntimeID:        firstNonEmpty(cost.RuntimeID, latency.RuntimeID, cand.RuntimeID),
			Engine:           firstNonEmpty(cost.Engine, latency.Engine, cand.Engine),
			HWClass:          firstNonEmpty(cost.HWClass, latency.HWClass),
			HardwareIdentity: firstNonEmpty(cost.HardwareIdentity, latency.HardwareIdentity),
			Currency:         p.Currency,

			PredictedLatencyMsPerUnit:                    predictedMs,
			PredictedSupplierLiabilityUSDPerVerifiedUnit: predictedCost,
			PredictedUnitsPerSec:                         predictedUPS,

			ActualLatencyMsPerUnit:                    actualMs,
			ActualSupplierLiabilityUSDPerVerifiedUnit: actualCost,
			ActualUnitsPerSec:                         actualUPS,
			LatencySourceBinding:                      normalizedEvidenceBinding(latency.SourceBinding),
			SupplierLiabilitySourceBinding:            normalizedEvidenceBinding(cost.SourceBinding),

			SupplierEntitlementUSDUnit: p.SupplierUSDPerUnit,
			BuyerPricePer1K:            p.BuyerPricePer1KUnits,

			QualityTier:           cand.QualityTier,
			VerificationContract:  cand.Verification,
			VerificationPassRate:  p.VerificationPassRate,
			ReliabilityMultiplier: p.ReliabilityMultiplier,
			Confidence:            p.Confidence,

			MercTrueNetUnavailable: !p.MercTrueNet.IsAvailable(),
			UnknownComponents:      append([]string(nil), p.UnknownComponents...),

			MercGrossPlatformAvailable: grossAvailable,
			MercGrossPlatformUSDUnit:   gross,
			MercGrossPlatformBasis:     governedGrossPlatformRefusal,

			LatencyRegretMsPerUnit:                     predictedMs - actualMs,
			SupplierLiabilityPredictionErrorUSDPerUnit: predictedCost - actualCost,
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

	// Actual winner comes only from the independently measured supplier-liability
	// selector input. A latency-only input must never fall back to a lifecycle
	// winner and masquerade as an observed selection.
	actualWinner := measuredDecision.Winner
	actualBasis := measuredDecision.Basis

	// Decision-level PREDICTION error: predicted−actual for the predicted
	// winner's metrics. If the prior named llama and llama was slower than
	// predicted, latency prediction-error is negative (predicted lower ms than
	// actual). Distinct from selection regret below.
	decisionLatencyRegret := 0.0
	decisionCostRegret := 0.0
	if row, ok := cells[predictedWinner]; ok {
		decisionLatencyRegret = row.LatencyRegretMsPerUnit
		decisionCostRegret = row.SupplierLiabilityPredictionErrorUSDPerUnit
	}

	// Selection regret is computed exclusively from the same full measured rows
	// that fed measuredDecision. The separate latency input may diagnose latency
	// and evaluate the prior, but it cannot retroactively score or change the
	// selector. This is a diagnostic axis, not a total-cost verdict.
	selectionLatencyRegret, selectionCostRegret := 0.0, 0.0
	bestLatencyCell, bestCostCell := "", ""
	if selected, ok := costs[actualWinner]; ok && selected.MedianMsPerUnit > 0 {
		bestMs := selected.MedianMsPerUnit
		bestLatencyCell = actualWinner
		for _, id := range cellIDs {
			other, exists := costs[id]
			if _, eligible := measuredSupplierLiability(costs, id); exists && eligible &&
				other.MedianMsPerUnit > 0 && other.MedianMsPerUnit < bestMs {
				bestMs = other.MedianMsPerUnit
				bestLatencyCell = id
			}
		}
		selectionLatencyRegret = selected.MedianMsPerUnit - bestMs
	}
	if selectedLiability, ok := measuredSupplierLiability(costs, actualWinner); ok {
		bestCost := selectedLiability
		bestCostCell = actualWinner
		for _, id := range cellIDs {
			if otherLiability, eligible := measuredSupplierLiability(costs, id); eligible && otherLiability < bestCost {
				bestCost = otherLiability
				bestCostCell = id
			}
		}
		selectionCostRegret = selectedLiability - bestCost
	}

	// Quality can say PASS only when at least two selector-eligible rows carry a
	// complete clean verification and terminal-outcome cohort. Zero samples are
	// unknown, not a silent pass.
	qualityOutcome := "UNVERIFIED_INSUFFICIENT_RELIABILITY_EVIDENCE"
	cleanReliabilityRows := 0
	for _, id := range cellIDs {
		cost, exists := costs[id]
		if !exists {
			continue
		}
		if cost.VerificationFails > 0 {
			qualityOutcome = fmt.Sprintf("VERIFICATION_FAILURES cell=%s fails=%d/%d",
				cost.CellID, cost.VerificationFails, cost.VerificationSamples)
			break
		}
		if cost.TerminalFails > 0 {
			qualityOutcome = fmt.Sprintf("TERMINAL_FAILURES cell=%s fails=%d/%d",
				cost.CellID, cost.TerminalFails, cost.TerminalAttempts)
			break
		}
		if _, eligible := measuredSupplierLiability(costs, id); eligible {
			cleanReliabilityRows++
		}
	}
	if cleanReliabilityRows >= 2 && qualityOutcome == "UNVERIFIED_INSUFFICIENT_RELIABILITY_EVIDENCE" {
		if actualsBinding(costs) == BindingBound {
			qualityOutcome = "PASS_EQUAL_CONTRACT"
		} else {
			qualityOutcome = "UNRESOLVED_UNBOUND_RELIABILITY_ACTUALS"
		}
	}

	// Confidence: minimum of the two measured projections (a chain is only as
	// confident as its weaker cell).
	confidence := 1.0
	nMeasured := 0
	for _, id := range cellIDs {
		if _, eligible := measuredSupplierLiability(costs, id); !eligible {
			continue
		}
		row := cells[id]
		nMeasured++
		if row.Confidence < confidence {
			confidence = row.Confidence
		}
	}
	if nMeasured == 0 {
		confidence = 0
	}

	selectionReason := selectionReasonFor(actualBasis, actualWinner, cells)
	economicsRefusal := measuredDecision.EconomicsRefusal
	if actualWinner == "" {
		selectionReason = "actual selection refused: no complete measured supplier-liability cohort; latency-only evidence cannot choose a cell"
		if economicsRefusal == "" {
			economicsRefusal = "selection refused: fewer than two complete clean supplier-liability/reliability rows"
		}
	}

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
		Currency:                            settlementCurrency,
		EligibleCells:                       eligible,
		ExcludedCells:                       excluded,
		PredictedWinner:                     predictedWinner,
		PredictedBasis:                      predictedBasis,
		ActualWinner:                        actualWinner,
		ActualBasis:                         actualBasis,
		DecidingTerm:                        actualBasis,
		SelectionReason:                     selectionReason,
		LatencyRegretMs:                     decisionLatencyRegret,
		SupplierLiabilityPredictionErrorUSD: decisionCostRegret,
		SelectionLatencyRegretMs:            selectionLatencyRegret,
		SelectionSupplierLiabilityRegretUSD: selectionCostRegret,
		BestAvailableLatencyCell:            bestLatencyCell,
		BestAvailableSupplierLiabilityCell:  bestCostCell,
		EconomicsRefusal:                    economicsRefusal,
		QualityOutcome:                      qualityOutcome,
		Confidence:                          confidence,
		RoutedCellID:                        shadow.RoutedCellID,
		ShadowCellID:                        actualWinner,
		Diverged:                            actualWinner != "" && actualWinner != shadow.RoutedCellID,
		Promoted:                            false,
		RoutingChanged:                      false,
		SelectionPolicy:                     shadowSelectionPolicy,
		RuntimeMatrixSHA:                    firstNonEmpty(shadow.RuntimeMatrixSHA, generatedRuntimeMatrixSHA256),
		SupplierLiabilityHWClass:            supplierLiabilityHWClass,
		SupplierLiabilityHardwareIdentity:   supplierLiabilityHardwareIdentity,
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
			ModelID:              catalogue.ModelID,
			JobType:              catalogue.JobType,
			ReferencePricePer1K:  catalogue.ReferencePricePer1K,
			SettlementPricePer1K: catalogue.SettlementPricePer1K,
			SupplierShare:        catalogue.SupplierShare,
			ScheduleSHA256:       catalogue.ScheduleSHA256,
			BoardSHA256:          catalogue.BoardSHA256,
			PriceFormula:         catalogue.PriceFormula,
			SettlementCurrency:   settlementCurrency,
		},
		HWClass:          decision.SupplierLiabilityHWClass,
		HardwareIdentity: decision.SupplierLiabilityHardwareIdentity,
		JobType:          shadow.JobType,
		ModelRef:         shadow.ModelRef,
		PriorThroughputClaim: map[string]any{
			"statement":   "prior measurement put llama.cpp ahead by ~2.1× throughput (lower bound 1.5×) on Metal embed",
			"ratio":       priorThroughputClaimRatio,
			"lower_bound": priorThroughputClaimLowerBound,
			"status":      priorClaimStatus(candleActualMs, llamaActualMs, latencyActualsBinding(latencies)),
			"note":        "this is the prediction under verification; only a BOUND actual is authoritative over it",
		},
		ActualsBinding: map[string]any{
			"status":                    governedActualsBinding(costs, latencies),
			"supplier_liability_status": actualsBinding(costs),
			"latency_status":            latencyActualsBinding(latencies),
			"why": "supplier liability/reliability and latency retain separate provenance; " +
				"the receipt's producer identity names who ran this arithmetic, not who produced either input",
			"consequence": "latency evidence may evaluate latency and a latency prior only; it cannot supply supplier payout, " +
				"fabricate clean verification/terminal outcomes, choose a cell, or make the combined receipt BOUND without independent BOUND supplier-liability evidence",
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
			"rank_by":                            "p50 median_ms_per_unit from the separate GovernedLatencyActual input; p95/p99 live in the latency receipt and are not re-derived here",
		},
		CostComparison: map[string]any{
			"currency": settlementCurrency,
			"candle_supplier_liability_usd_per_verified_unit": candleActualCost,
			"llama_supplier_liability_usd_per_verified_unit":  llamaActualCost,
			"absolute_delta_usd_per_unit":                     llamaActualCost - candleActualCost,
			"tie":                                             supplierLiabilitiesTieUSD(candleActualCost, llamaActualCost),
			"authority":                                       supplierEntitlementPolicyCancelled,
			"note":                                            "supplier entitlement is units/1000 × price × share; unitsPerSec cancels; equal reliability ⇒ equal supplier liability; this is not total cost",
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
			"does not treat latency-only evidence as supplier-liability, reliability, or selector evidence",
			"does not authorise a promotion receipt; that is a separate governed act",
		},
		Limitations: []string{
			hardwareScopeLimitation(supplierLiabilityHWClass),
			"Host load is recorded because the machine is not perfectly quiet.",
			"Energy partial uses ASSUMED watts and defaulted electricity.",
			"Storage, egress, utilization, refund risk remain unknown.",
			"A latency ratio is never the headline; the absolute delta is stated first.",
		},
	}
	return out, nil
}

const governedGrossPlatformRefusal = "REFUSED: buyer price is denominated per 1,000 frozen settlement input units, while measured supplier payout is denominated per accepted output record; no buyer-minus-supplier gross exists until frozen buyer charge per accepted output (or exact billable-input/output geometry) is carried into this comparison"

// mercGrossPlatformPerUnit deliberately refuses projection-only gross. The
// projection has a settlement price per input unit and a payout per accepted
// output record, but no frozen ratio between those denominators. Currency
// equality is necessary and already enforced; it is not sufficient to make
// dimensionally different amounts subtractable.
func mercGrossPlatformPerUnit(_ CellEconomicsProjection) (amount float64, available bool) {
	return 0, false
}

func validateGovernedSupplierCurrencies(
	shadow ShadowSelection,
	costs map[string]MeasuredSupplierLiabilityProxy,
	catalogue CataloguePriceAuthority,
	settlementCurrency string,
) error {
	for _, candidate := range shadow.Considered {
		cost, exists := costs[candidate.CellID]
		if !exists {
			continue
		}
		if !cellEconomicsSupplierCurrencyMatches(
			cost.Currency, settlementCurrency, catalogue) {
			return fmt.Errorf(
				"governed comparison supplier-liability currency %q for cell %q does not match settlement currency %q; cross-currency ranking is refused",
				cost.Currency, candidate.CellID, settlementCurrency)
		}
	}
	return nil
}

func hardwareScopeLimitation(hwClass string) string {
	if hwClass = strings.TrimSpace(hwClass); hwClass != "" {
		return fmt.Sprintf("One host, one hardware class (%s); not a fleet claim.", hwClass)
	}
	return "Supplier-liability hardware comparison scope unavailable; no hardware-class or fleet claim."
}

func validateGovernedLatencyActuals(
	shadow ShadowSelection,
	costs map[string]MeasuredSupplierLiabilityProxy,
	latencies map[string]GovernedLatencyActual,
) error {
	for _, candidate := range shadow.Considered {
		latency, ok := latencies[candidate.CellID]
		if !ok {
			return fmt.Errorf("governed comparison has no explicit latency actual for cell %q", candidate.CellID)
		}
		if latency.CellID != candidate.CellID {
			return fmt.Errorf("governed comparison latency key %q carries cell id %q",
				candidate.CellID, latency.CellID)
		}
		if latency.Samples < governedLatencyMinSamples || latency.Units <= 0 ||
			latency.MedianMsPerUnit <= 0 || math.IsNaN(latency.MedianMsPerUnit) ||
			math.IsInf(latency.MedianMsPerUnit, 0) {
			return fmt.Errorf("governed comparison latency actual for cell %q is incomplete", candidate.CellID)
		}
		if candidate.RuntimeID != "" && latency.RuntimeID != candidate.RuntimeID {
			return fmt.Errorf("governed comparison latency runtime %q does not match candidate %q for cell %q",
				latency.RuntimeID, candidate.RuntimeID, candidate.CellID)
		}
		if candidate.Engine != "" && latency.Engine != candidate.Engine {
			return fmt.Errorf("governed comparison latency engine %q does not match candidate %q for cell %q",
				latency.Engine, candidate.Engine, candidate.CellID)
		}
		if cost, exists := costs[candidate.CellID]; exists && cost.HWClass != "" &&
			(latency.HWClass != cost.HWClass ||
				latency.HardwareIdentity != cost.HardwareIdentity) {
			return fmt.Errorf("governed comparison latency hardware %q/%q does not match supplier-liability hardware %q/%q for cell %q",
				latency.HWClass, latency.HardwareIdentity, cost.HWClass,
				cost.HardwareIdentity, candidate.CellID)
		}
	}
	return nil
}

// exactSupplierLiabilityHardwareScope derives the exact class/device scope from the complete selector
// inputs themselves. A shadow label is only a consistency check; it cannot fill
// a missing measurement scope or silently restore the former hard-coded Apple
// default.
func exactSupplierLiabilityHardwareScope(
	costs map[string]MeasuredSupplierLiabilityProxy,
	cellIDs []string,
	declaredClass, declaredIdentity string,
) (string, string, error) {
	commonClass, commonIdentity := "", ""
	eligibleRows := 0
	for _, id := range cellIDs {
		if _, eligible := measuredSupplierLiability(costs, id); !eligible {
			continue
		}
		hw := strings.TrimSpace(costs[id].HWClass)
		identity := strings.TrimSpace(costs[id].HardwareIdentity)
		if hw == "" || !validCanonicalHardwareIdentity(identity) {
			return "", "", fmt.Errorf(
				"governed comparison supplier-liability row %q has no exact hardware class/device identity", id)
		}
		if commonClass == "" {
			commonClass, commonIdentity = hw, identity
		} else if commonClass != hw || commonIdentity != identity {
			return "", "", fmt.Errorf(
				"governed comparison supplier-liability hardware scope is mixed: %q/%q and %q/%q",
				commonClass, commonIdentity, hw, identity)
		}
		eligibleRows++
	}
	// Fewer than two clean rows establish no comparison scope. Leave it empty;
	// callers surface the independent selection/quality refusal.
	if eligibleRows < 2 {
		return "", "", nil
	}
	declaredClass = strings.TrimSpace(declaredClass)
	declaredIdentity = strings.TrimSpace(declaredIdentity)
	if (declaredClass != "" && declaredClass != commonClass) ||
		(declaredIdentity != "" && declaredIdentity != commonIdentity) {
		return "", "", fmt.Errorf(
			"governed comparison declared supplier-liability hardware %q/%q does not match measured scope %q/%q",
			declaredClass, declaredIdentity, commonClass, commonIdentity)
	}
	if (declaredClass == "") != (declaredIdentity == "") {
		return "", "", errors.New(
			"governed comparison declared only one half of the exact hardware scope")
	}
	return commonClass, commonIdentity, nil
}

func candleActualBaseline(latencies map[string]GovernedLatencyActual) float64 {
	if c, ok := latencies[candleEmbedCell]; ok && c.MedianMsPerUnit > 0 {
		return c.MedianMsPerUnit
	}
	// Fall back to any candle-engine cell.
	for _, c := range latencies {
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
func actualsBinding(costs map[string]MeasuredSupplierLiabilityProxy) string {
	if len(costs) == 0 {
		return BindingUnbound
	}
	statuses := make([]string, 0, len(costs))
	for _, c := range costs {
		statuses = append(statuses, c.SourceBinding)
	}
	return weakestEvidenceBinding(statuses...)
}

func latencyActualsBinding(latencies map[string]GovernedLatencyActual) string {
	if len(latencies) == 0 {
		return BindingUnbound
	}
	statuses := make([]string, 0, len(latencies))
	for _, latency := range latencies {
		statuses = append(statuses, latency.SourceBinding)
	}
	return weakestEvidenceBinding(statuses...)
}

func governedActualsBinding(
	costs map[string]MeasuredSupplierLiabilityProxy,
	latencies map[string]GovernedLatencyActual,
) string {
	return weakestEvidenceBinding(actualsBinding(costs), latencyActualsBinding(latencies))
}

func normalizedEvidenceBinding(status string) string {
	switch strings.ToUpper(strings.TrimSpace(status)) {
	case BindingBound:
		return BindingBound
	case BindingWithdrawn:
		return BindingWithdrawn
	case BindingSuperseded:
		return BindingSuperseded
	case BindingUnbound:
		return BindingUnbound
	default:
		return BindingUnbound
	}
}

// weakestEvidenceBinding preserves terminal withdrawal/supersession instead of
// flattening them to UNBOUND. Every non-BOUND state refuses derived authority;
// the more specific status explains whether a replacement or a new authority id
// is required.
func weakestEvidenceBinding(statuses ...string) string {
	weakest := BindingBound
	for _, status := range statuses {
		switch normalizedEvidenceBinding(status) {
		case BindingWithdrawn:
			return BindingWithdrawn
		case BindingSuperseded:
			if weakest != BindingUnbound {
				weakest = BindingSuperseded
			}
		case BindingUnbound:
			weakest = BindingUnbound
		}
	}
	return weakest
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
	case selectionBasisThroughputEqualLiability:
		return fmt.Sprintf(
			"supplier liability ties by catalogue arithmetic; cell %s wins on more throughput at equal supplier liability — not a cost win",
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
