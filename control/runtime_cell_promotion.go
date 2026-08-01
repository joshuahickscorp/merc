package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"time"
)

// The gate a challenger cell has to clear before it may be routed, and the
// receipt that records the argument.
//
// runtime_activation_policies already carries `promotion_receipt` and
// `rollback_target`, and already refuses a routable entry with an empty receipt.
// What did not exist was anything that DERIVED a receipt from evidence: the
// column would accept the path of a benchmark file that nobody had compared
// against the incumbent, on a scope nobody had pinned, with no rollback target
// named. So this file is the gate, not a second activation mechanism — its output
// is the string that column stores.
//
// The gate is deliberately hostile to its own challenger. Every rule below exists
// because the opposite rule would let a cell promote itself:
//
//   - measured on the SAME hardware class, or the comparison is of two machines;
//   - both sides above minCellCostSamples, or the winner is sampling noise;
//   - zero verification failures for the challenger, because a cheaper unit that
//     fails verification is not a cheaper unit;
//   - a margin, not merely "less", because two costs within noise of each other
//     do not justify moving production traffic;
//   - the incumbent must be the cell currently ROUTED, so a promotion is always
//     argued against what buyers are actually getting;
//   - a rollback target names the exact policy revision to return to.

// promotionCostMarginFraction is how much cheaper a challenger must be.
//
// Ten percent. Below that the difference is not distinguishable from thermal
// state, page-cache residency and queue depth on a shared host, and a selector
// that reshuffles production traffic for a 2% paper win spends more in cold
// starts than it saves.
const promotionCostMarginFraction = 0.10

// promotionGateVersion is bound into every receipt. A receipt produced under one
// gate must never be read as though it had cleared a later, stricter one.
const promotionGateVersion = "cell-promotion-gate-v2"

// promotionThroughputMarginFraction is how much FASTER a challenger must be to be
// promoted when the two cells cost the same per unit.
//
// This arm exists because the first real cohort found that they always do. The
// supplier payout is priced per MODEL — catalogue price times units times the
// supplier share — so two cells serving one model at one price have identical
// verified-outcome cost per unit by construction, and the regret between them is
// structurally zero however fast either one is. Forty real executions measured
// exactly that: 0.194 USD/unit on both cells, mean regret 0.000.
//
// So a promotion between same-price cells cannot be a COST argument, and calling
// it one would be inventing a saving. It is a CAPACITY argument: the faster cell
// delivers more verified units per host-hour, which pays the supplier more per
// hour at the same buyer price and gives Merc more throughput per machine. The
// receipt names which argument was made, because they are not interchangeable.
//
// Twenty-five percent, not ten: the price arm's margin protects against noise in
// a dollar figure, and this one has to protect against noise in a duration
// measured on a shared host with thermal state and queue depth in it.
const promotionThroughputMarginFraction = 0.25

// Promotion bases. Every receipt records exactly one.
const (
	// promotionBasisCost is a cheaper verified unit.
	promotionBasisCost = "CHEAPER_VERIFIED_UNIT"
	// promotionBasisThroughput is the same verified unit produced faster at the
	// same price. A capacity gain, not a saving.
	promotionBasisThroughput = "MORE_THROUGHPUT_AT_EQUAL_PRICE"
)

// pricesTieWithin is the fraction inside which two per-unit costs are the same
// number for this purpose. Not exact equality: the two costs are computed from
// summed NUMERIC(12,6) payouts divided by summed unit counts, so a rounding
// difference of a few micro-USD in a large sample is a tie and not a saving.
const pricesTieWithin = 0.005

// CellPromotionScope is the exact scope a promotion is valid for.
//
// merc.md §7 requires promotion scope to name workload, model revision, artifact
// revision, quality contract, runtime cell, hardware class and latency policy.
// Anything wider promotes a cell for traffic it was never measured on.
type CellPromotionScope struct {
	JobType       string `json:"job_type"`
	ModelRef      string `json:"model_ref"`
	ModelRevision string `json:"model_revision"`
	QualityTier   string `json:"quality_tier"`
	Verification  string `json:"verification_contract"`
	HWClass       string `json:"hw_class"`
	LatencyClass  string `json:"latency_class"`
	RuntimeID     string `json:"runtime_id"`
	CellID        string `json:"cell_id"`
}

// CellPromotionEvidence is the measured argument for one promotion.
type CellPromotionEvidence struct {
	GateVersion    string             `json:"gate_version"`
	EvaluatedAt    time.Time          `json:"evaluated_at"`
	Scope          CellPromotionScope `json:"scope"`
	IncumbentCell  string             `json:"incumbent_cell"`
	ChallengerCost MeasuredCellCost   `json:"challenger_cost"`
	IncumbentCost  MeasuredCellCost   `json:"incumbent_cost"`

	ChallengerVerifiedUSDPerUnit float64 `json:"challenger_verified_usd_per_unit"`
	IncumbentVerifiedUSDPerUnit  float64 `json:"incumbent_verified_usd_per_unit"`
	SavingFraction               float64 `json:"saving_fraction"`
	RequiredMarginFraction       float64 `json:"required_margin_fraction"`

	// Basis names which argument this promotion makes: a cheaper verified unit, or
	// the same unit produced faster at the same price. They are not
	// interchangeable, and a receipt that did not say which would let a capacity
	// gain be read as a saving.
	Basis                  string  `json:"basis"`
	ThroughputGainFraction float64 `json:"throughput_gain_fraction"`

	// LatencyRatio is the challenger's median milliseconds per unit over the
	// incumbent's: above 1 it is slower. Always reported, even when the trade is
	// permitted, because a promotion that saves money by being slower is a
	// decision someone should see rather than infer.
	LatencyRatio float64 `json:"latency_ratio_challenger_over_incumbent"`

	// Regret is the recorded selector regret for this scope. A promotion argued
	// without it would be arguing from a benchmark; with it, the argument is that
	// production decisions have been measurably paying for the wrong cell.
	Regret SelectorRegret `json:"selector_regret"`

	// RollbackTargetRevision is the activation revision to return to, resolved
	// before the promotion is applied rather than after something breaks.
	RollbackTargetRevision int64 `json:"rollback_target_revision"`

	// RuntimeMatrixSHA256 and PolicyRevision pin the authority the evidence was
	// gathered under.
	RuntimeMatrixSHA256 string `json:"runtime_matrix_sha256"`
	PolicyRevision      int64  `json:"policy_revision"`

	// Refusals is empty exactly when the gate passed. It is a list rather than a
	// single reason so one evaluation reports every failing rule; fixing them one
	// round-trip at a time is how a gate gets worn down.
	Refusals []string `json:"refusals"`

	// UnknownCostComponents restates what the cost comparison does not contain.
	// A promotion receipt that hid this would be claiming a total-cost win.
	UnknownCostComponents []string `json:"unknown_cost_components"`
}

// Passed reports whether every rule held.
func (e CellPromotionEvidence) Passed() bool { return len(e.Refusals) == 0 }

// Digest is the receipt identity: a SHA-256 over the canonical evidence.
//
// Computed over the evidence with the digest field itself absent, which is why it
// is a method rather than a stored column — a digest that covered itself would be
// uncomputable, and one stored beside the evidence could disagree with it.
func (e CellPromotionEvidence) Digest() (string, error) {
	canonical, err := json.Marshal(e)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:]), nil
}

// ReceiptRef is what runtime_activation_policies.promotion_receipt stores.
func (e CellPromotionEvidence) ReceiptRef() (string, error) {
	digest, err := e.Digest()
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s:%s:%s", promotionGateVersion, e.Scope.CellID, digest), nil
}

// EvaluateCellPromotion decides whether challenger may replace incumbent for one
// scope, and returns the evidence either way.
//
// A refusal is returned as evidence with reasons, not as an error: the difference
// between "the gate says no, here is what would change that" and "the gate could
// not run" matters to an operator, and collapsing both into an error loses it.
// err is reserved for the second.
func (s *Store) EvaluateCellPromotion(
	ctx context.Context, scope CellPromotionScope, incumbentCell string, now time.Time,
) (CellPromotionEvidence, error) {
	if scope.CellID == "" || incumbentCell == "" {
		return CellPromotionEvidence{}, fmt.Errorf("promotion needs a challenger and an incumbent cell")
	}
	if scope.CellID == incumbentCell {
		return CellPromotionEvidence{}, fmt.Errorf("challenger and incumbent are the same cell %q", scope.CellID)
	}
	activation := currentActivation()
	evidence := CellPromotionEvidence{
		GateVersion:   promotionGateVersion,
		EvaluatedAt:   now.UTC(),
		Scope:         scope,
		IncumbentCell: incumbentCell,
		// Overwritten once the arithmetic says which argument is available.
		Basis:                  promotionBasisCost,
		RequiredMarginFraction: promotionCostMarginFraction,
		RuntimeMatrixSHA256:    generatedRuntimeMatrixSHA256,
		PolicyRevision:         activation.PolicyRevision,
		RollbackTargetRevision: activation.PolicyRevision,
		UnknownCostComponents:  unknownCostComponents(),
	}

	costScope := cellCostScope{JobType: scope.JobType, ModelRef: scope.ModelRef, HWClass: scope.HWClass}
	regret, costs, err := s.SelectorRegretForScope(ctx, costScope)
	if err != nil {
		return CellPromotionEvidence{}, err
	}
	evidence.Regret = regret
	evidence.ChallengerCost = costs[scope.CellID]
	evidence.IncumbentCost = costs[incumbentCell]

	refuse := func(format string, args ...any) {
		evidence.Refusals = append(evidence.Refusals, fmt.Sprintf(format, args...))
	}

	challengerCost, challengerOK := measuredCost(costs, scope.CellID)
	incumbentCost, incumbentOK := measuredCost(costs, incumbentCell)
	evidence.ChallengerVerifiedUSDPerUnit = challengerCost
	evidence.IncumbentVerifiedUSDPerUnit = incumbentCost

	if !challengerOK {
		refuse("challenger %s has no measured verified-outcome cost on %s (%d of %d samples needed)",
			scope.CellID, scope.HWClass, evidence.ChallengerCost.Samples, minCellCostSamples)
	}
	if !incumbentOK {
		refuse("incumbent %s has no measured verified-outcome cost on %s (%d of %d samples needed)",
			incumbentCell, scope.HWClass, evidence.IncumbentCost.Samples, minCellCostSamples)
	}
	if evidence.ChallengerCost.VerificationFails > 0 {
		refuse("challenger %s failed verification %d of %d times; a cheaper rejected unit is not cheaper",
			scope.CellID, evidence.ChallengerCost.VerificationFails, evidence.ChallengerCost.VerificationSamples)
	}
	if evidence.ChallengerCost.TerminalFails > 0 {
		refuse("challenger %s failed outright on %d of %d terminal attempts; work that never "+
			"produced a result is not cheap work",
			scope.CellID, evidence.ChallengerCost.TerminalFails,
			evidence.ChallengerCost.TerminalAttempts)
	}
	if evidence.ChallengerCost.VerificationSamples == 0 {
		refuse("challenger %s has no verified sample; cost without a verification outcome is unproven",
			scope.CellID)
	}
	if challengerOK && incumbentOK {
		evidence.SavingFraction = (incumbentCost - challengerCost) / incumbentCost
		// Which argument is even available decides which margin applies. A tie on
		// price is the normal case for two cells serving one model, so treating it
		// as a failed cost argument would refuse every same-price promotion on the
		// grounds that it saved nothing — when what it offers is capacity.
		tied := evidence.SavingFraction > -pricesTieWithin &&
			evidence.SavingFraction < pricesTieWithin
		switch {
		case tied:
			evidence.Basis = promotionBasisThroughput
			evidence.RequiredMarginFraction = promotionThroughputMarginFraction
			gain := 0.0
			if in, out := evidence.ChallengerCost.MedianMsPerUnit, evidence.IncumbentCost.MedianMsPerUnit; in > 0 && out > 0 {
				gain = (out - in) / out
			}
			evidence.ThroughputGainFraction = gain
			if gain < promotionThroughputMarginFraction {
				refuse("challenger %s costs the same per unit (%.2f%% apart) and is only "+
					"%.2f%% faster, below the %.0f%% throughput margin; a same-price "+
					"promotion has to buy capacity because it cannot buy a saving",
					scope.CellID, evidence.SavingFraction*100, gain*100,
					promotionThroughputMarginFraction*100)
			}
		default:
			evidence.Basis = promotionBasisCost
			if evidence.SavingFraction < promotionCostMarginFraction {
				refuse("challenger %s saves %.2f%%, below the required %.0f%% margin",
					scope.CellID, evidence.SavingFraction*100, promotionCostMarginFraction*100)
			}
		}
	}
	// Cost is not the only contract. A cheaper cell that is slower per unit is a
	// legitimate trade for batch work, whose deadline absorbs it, and is not a
	// trade at all where latency IS the product.
	if in, out := evidence.ChallengerCost.MedianMsPerUnit, evidence.IncumbentCost.MedianMsPerUnit; in > 0 && out > 0 {
		evidence.LatencyRatio = in / out
		if evidence.LatencyRatio > 1 && scopeIsLatencySensitive(scope.LatencyClass) {
			refuse("challenger %s is %.0f%% slower per unit (%.3f ms vs %.3f ms) and %s is a "+
				"latency class where that is the product, not a trade",
				scope.CellID, (evidence.LatencyRatio-1)*100, in, out, scope.LatencyClass)
		}
	}
	if regret.ScoredDecisions == 0 {
		refuse("no recorded selector decision for %s scored both cells; a promotion needs production decisions, not a benchmark",
			costScope)
	}

	// The scope must describe a cell this authority actually declares, with the
	// quality and verification contract the scope claims. Otherwise a promotion
	// could be argued for a contract the cell does not sell.
	if err := verifyPromotionScopeAgainstAuthority(activation, scope, incumbentCell, refuse); err != nil {
		return CellPromotionEvidence{}, err
	}
	sort.Strings(evidence.Refusals)
	return evidence, nil
}

// scopeIsLatencySensitive reports whether the scope's latency class is one where
// a slower cell cannot be promoted however cheap it is.
//
// Accepts both spellings the tree uses. The batch classifier freezes
// standard_batch, priority_queue and trusted_supply; the traffic-class table
// speaks INTERACTIVE, BATCH_PRIORITY, BATCH_STANDARD and BACKGROUND. A promotion
// scope may carry either, and resolving only one of them would silently treat a
// realtime scope as ordinary batch work.
func scopeIsLatencySensitive(latencyClass string) bool {
	if latencyClass == string(TrafficInteractive) {
		return true
	}
	return TrafficClassForWorkload(latencyClass, 0) == TrafficInteractive
}

// verifyPromotionScopeAgainstAuthority checks the scope against the runtime
// authority: the challenger must exist, declare the claimed contracts, and be
// reachable by directed routing (that is how it earned measurements at all), and
// the incumbent must be the cell currently routable for this workload.
func verifyPromotionScopeAgainstAuthority(
	activation *runtimeActivation, scope CellPromotionScope, incumbentCell string,
	refuse func(string, ...any),
) error {
	var challengerFound, incumbentFound bool
	for _, profile := range activation.profiles() {
		for _, cell := range profile.Cells {
			if cell.Job != scope.JobType || cell.Model != scope.ModelRef {
				continue
			}
			switch cell.ID {
			case scope.CellID:
				challengerFound = true
				if profile.RuntimeID != scope.RuntimeID {
					refuse("challenger %s belongs to runtime %s, scope claims %s",
						cell.ID, profile.RuntimeID, scope.RuntimeID)
				}
				if tier := cell.qualityTierFor(profile); tier != scope.QualityTier {
					refuse("challenger %s sells quality tier %s, scope claims %s",
						cell.ID, tier, scope.QualityTier)
				}
				if cell.Verification != scope.Verification {
					refuse("challenger %s sells verification %s, scope claims %s",
						cell.ID, cell.Verification, scope.Verification)
				}
				if !cell.ReachableByDirectedRouting(profile) {
					refuse("challenger %s is not reachable by directed routing, so its measurements cannot be attributed to it",
						cell.ID)
				}
			case incumbentCell:
				incumbentFound = true
				if !cell.Routable(profile) {
					refuse("incumbent %s is not routable; a promotion must be argued against the cell buyers are served by",
						cell.ID)
				}
			}
		}
	}
	if !challengerFound {
		refuse("challenger %s is not declared for %s/%s by this authority",
			scope.CellID, scope.JobType, scope.ModelRef)
	}
	if !incumbentFound {
		refuse("incumbent %s is not declared for %s/%s by this authority",
			incumbentCell, scope.JobType, scope.ModelRef)
	}
	return nil
}
