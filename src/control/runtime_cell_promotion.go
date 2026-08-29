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
//   - both sides above minSupplierLiabilitySamples, or the winner is sampling noise;
//   - zero retries plus complete clean verification and terminal evidence for
//     both sides: the accepted supplier payout does not price reliability, and
//     a retried, rejected, or failed attempt cannot be converted into fictional
//     supplier money;
//   - unequal supplier liability can never masquerade as total-cost evidence
//     while any platform-cost component is unknown;
//   - the incumbent must be the cell currently ROUTED, so a promotion is always
//     argued against what buyers are actually getting;
//   - a rollback target names the exact policy revision to return to.

// promotionGateVersion is bound into every receipt. A receipt produced under one
// gate must never be read as though it had cleared a later, stricter one.
const promotionGateVersion = "cell-promotion-gate-v4"

// promotionThroughputMarginFraction is how much FASTER a challenger must be to be
// promoted when the two cells have equal supplier liability per verified unit.
//
// This arm exists because the first real cohort found that they always do. The
// supplier payout is priced per MODEL — catalogue price times units times the
// supplier share — so two equally reliable cells serving one model have
// identical supplier liability per verified unit by construction.
//
// So a promotion between equal-liability cells cannot be a COST argument, and calling
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
	// promotionBasisSupplierLiabilityOnly records an unequal-liability proxy that
	// cannot authorize a total-cost promotion.
	promotionBasisSupplierLiabilityOnly = "SUPPLIER_LIABILITY_PROXY_ONLY_COST_REFUSED"
	// promotionBasisThroughput is the same verified unit produced faster at equal
	// supplier liability. A capacity gain, not a cost saving.
	promotionBasisThroughput = "MORE_THROUGHPUT_AT_EQUAL_SUPPLIER_LIABILITY"
)

// The current observation model has independently aggregated cell executions
// plus a shadow decision that names two considered cells. It does not persist a
// matched incumbent/challenger execution pair, a shared input/cohort digest, or
// a durable join proving both measurements came from that same input. Reporting
// the aggregates remains useful; treating them as paired causal evidence does
// not. Version 4 therefore refuses every promotion until that authority exists.
const promotionMatchedPairAuthorityRefusal = "promotion cannot pass: no durable matched incumbent/challenger execution-pair authority binds both cells to the same input/cohort digest; shadow consideration plus independently aggregated jobs is not matched-pair evidence"

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
	Tier          string `json:"tier"`
	QualityTier   string `json:"quality_tier"`
	Verification  string `json:"verification_contract"`
	HWClass       string `json:"hw_class"`
	// HardwareIdentity is the exact measured device generation/model. HWClass
	// remains the capacity family, but cannot stand in for physical identity.
	HardwareIdentity string `json:"hardware_identity"`
	LatencyClass     string `json:"latency_class"`
	RuntimeID        string `json:"runtime_id"`
	CellID           string `json:"cell_id"`

	// These fields are derived by EvaluateCellPromotion from the current
	// append-only catalogue/runtime policy and bound into the receipt. They are
	// not operator-supplied filters: accepting caller guesses here would let an
	// evaluation claim an epoch different from the one it actually queried.
	Currency                string    `json:"currency"`
	CatalogueScheduleSHA256 string    `json:"catalogue_schedule_sha256"`
	RuntimeMatrixSHA256     string    `json:"runtime_matrix_sha256"`
	SelectionPolicy         string    `json:"selection_policy"`
	PolicyRevision          int64     `json:"policy_revision"`
	ObservedAfter           time.Time `json:"observed_after"`
	ObservedBefore          time.Time `json:"observed_before"`
}

// CellPromotionEvidence is the measured argument for one promotion.
type CellPromotionEvidence struct {
	GateVersion                 string                         `json:"gate_version"`
	EvaluatedAt                 time.Time                      `json:"evaluated_at"`
	Scope                       CellPromotionScope             `json:"scope"`
	IncumbentCell               string                         `json:"incumbent_cell"`
	ChallengerSupplierLiability MeasuredSupplierLiabilityProxy `json:"challenger_supplier_liability_proxy"`
	IncumbentSupplierLiability  MeasuredSupplierLiabilityProxy `json:"incumbent_supplier_liability_proxy"`

	ChallengerSupplierLiabilityUSDPerVerifiedUnit float64 `json:"challenger_supplier_liability_usd_per_verified_unit"`
	IncumbentSupplierLiabilityUSDPerVerifiedUnit  float64 `json:"incumbent_supplier_liability_usd_per_verified_unit"`
	SupplierLiabilityReductionFraction            float64 `json:"supplier_liability_reduction_fraction"`
	RequiredMarginFraction                        float64 `json:"required_margin_fraction"`

	// Basis names which argument this evaluation makes: an incomplete unequal-
	// liability proxy that must refuse, or the same unit produced faster at equal
	// supplier liability. They are not
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
	LiabilityRegret SelectorLiabilityRegret `json:"selector_supplier_liability_regret"`

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

	// UnknownPlatformCostComponents restates what the proxy does not contain.
	// A promotion receipt that hid this would be claiming a total-cost win.
	UnknownPlatformCostComponents []string `json:"unknown_platform_cost_components"`
}

// Passed reports whether every rule held.
func (e CellPromotionEvidence) Passed() bool { return len(e.Refusals) == 0 }

// validateMeasuredProxyCurrentExecutionIdentity prevents historical or
// directed observations from lending authority to a different current engine
// build or device. Logical model/cell identity is insufficient: supplier
// duration and the active-hour floor are physical properties of these exact
// bytes on this exact machine generation.
func validateMeasuredProxyCurrentExecutionIdentity(
	proxy MeasuredSupplierLiabilityProxy, cellID string,
) error {
	profile, _, benchmark, err := currentRuntimeCellBenchmarkIdentity(cellID)
	if err != nil {
		return err
	}
	if proxy.Engine != profile.Engine ||
		proxy.ExecutionBuildHash != benchmark.EngineBuildHash ||
		proxy.ExecutionBuildIdentityPolicy != benchmark.EngineBuildIdentityPolicy ||
		proxy.HWClass != benchmark.HWClass ||
		proxy.HardwareIdentity != benchmark.HardwareIdentity {
		return fmt.Errorf(
			"observations bind execution %s/%s@%s on %s/%s, current cell %s binds %s/%s@%s on %s/%s",
			proxy.Engine, proxy.ExecutionBuildHash, proxy.ExecutionBuildIdentityPolicy, proxy.HWClass,
			proxy.HardwareIdentity, cellID, profile.Engine,
			benchmark.EngineBuildHash, benchmark.EngineBuildIdentityPolicy,
			benchmark.HWClass, benchmark.HardwareIdentity)
	}
	return nil
}

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

// RecordCellPromotionEvaluation persists the complete refusal-preserving
// evidence receipt. It is deliberately separate from activation: an operator
// can inspect and review a receipt without granting the challenger traffic.
// The digest is the idempotency key, so retries of the same evaluation cannot
// rewrite its historical observation or create a second identity for it.
func (s *Store) RecordCellPromotionEvaluation(ctx context.Context, evidence CellPromotionEvidence) (bool, error) {
	raw, err := json.Marshal(evidence)
	if err != nil {
		return false, err
	}
	digest, err := evidence.Digest()
	if err != nil {
		return false, err
	}
	receiptRef, err := evidence.ReceiptRef()
	if err != nil {
		return false, err
	}
	scope, err := json.Marshal(evidence.Scope)
	if err != nil {
		return false, err
	}
	result, err := s.pool.Exec(ctx, `
		INSERT INTO runtime_cell_promotion_evaluations
		  (evidence_sha256, promotion_receipt_ref, gate_version, scope_json,
		   incumbent_cell, challenger_cell, passed, policy_revision,
		   runtime_matrix_sha256, evaluated_at, evidence_json)
		VALUES ($1,$2,$3,$4::jsonb,$5,$6,$7,$8,$9,$10,$11::jsonb)
		ON CONFLICT (evidence_sha256) DO NOTHING`,
		digest, receiptRef, evidence.GateVersion, string(scope), evidence.IncumbentCell,
		evidence.Scope.CellID, evidence.Passed(), evidence.PolicyRevision,
		evidence.RuntimeMatrixSHA256, evidence.EvaluatedAt, string(raw))
	if err != nil {
		return false, err
	}
	return result.RowsAffected() == 1, nil
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
	now = now.UTC()
	activation := currentActivation()
	scope.RuntimeMatrixSHA256 = generatedRuntimeMatrixSHA256
	scope.SelectionPolicy = shadowSelectionPolicy
	scope.PolicyRevision = activation.PolicyRevision
	scope.ObservedBefore = now
	scope.ObservedAfter = now.Add(-supplierLiabilityObservationWindow)

	// Currency and catalogue schedule are derived from immutable jobs/shadow
	// decisions in the otherwise exact bounded cohort, never accepted from the
	// operator and never re-read from today's mutable model pointer. More than
	// one economic epoch is a refusal rather than an invitation to pool them.
	epochScope := supplierLiabilityScope{
		JobType: scope.JobType, ModelRef: scope.ModelRef, HWClass: scope.HWClass,
		HardwareIdentity: scope.HardwareIdentity,
		Tier:             scope.Tier, RuntimeMatrixSHA256: scope.RuntimeMatrixSHA256,
		ModelRevision: scope.ModelRevision, QualityTier: scope.QualityTier,
		Verification: scope.Verification, LatencyClass: scope.LatencyClass,
		SelectionPolicy: scope.SelectionPolicy, PolicyRevision: scope.PolicyRevision,
		ObservedAfter: scope.ObservedAfter, ObservedBefore: scope.ObservedBefore,
	}
	currency, schedule, epochErr := s.resolveSupplierLiabilityEconomicEpoch(ctx, epochScope)
	if epochErr == nil {
		scope.Currency = currency
		scope.CatalogueScheduleSHA256 = schedule
	}
	evidence := CellPromotionEvidence{
		GateVersion:   promotionGateVersion,
		EvaluatedAt:   now,
		Scope:         scope,
		IncumbentCell: incumbentCell,
		// Overwritten only when equal liability makes the throughput arm available.
		Basis:                         promotionBasisSupplierLiabilityOnly,
		RequiredMarginFraction:        0,
		RuntimeMatrixSHA256:           generatedRuntimeMatrixSHA256,
		PolicyRevision:                activation.PolicyRevision,
		RollbackTargetRevision:        activation.PolicyRevision,
		UnknownPlatformCostComponents: unknownPlatformCostComponents(),
	}

	refuse := func(format string, args ...any) {
		evidence.Refusals = append(evidence.Refusals, fmt.Sprintf(format, args...))
	}
	refuse("%s", promotionMatchedPairAuthorityRefusal)
	if epochErr != nil {
		refuse("%v", epochErr)
	}

	liabilityScope := supplierLiabilityScope{
		JobType: scope.JobType, ModelRef: scope.ModelRef, HWClass: scope.HWClass,
		HardwareIdentity: scope.HardwareIdentity,
		Tier:             scope.Tier, Currency: scope.Currency,
		CatalogueScheduleSHA256: scope.CatalogueScheduleSHA256,
		RuntimeMatrixSHA256:     scope.RuntimeMatrixSHA256,
		ModelRevision:           scope.ModelRevision,
		QualityTier:             scope.QualityTier, Verification: scope.Verification,
		LatencyClass: scope.LatencyClass, SelectionPolicy: scope.SelectionPolicy,
		PolicyRevision: scope.PolicyRevision,
		ObservedAfter:  scope.ObservedAfter, ObservedBefore: scope.ObservedBefore,
		IncumbentCell: incumbentCell, ChallengerCell: scope.CellID,
	}
	liabilities := map[string]MeasuredSupplierLiabilityProxy{}
	var regret SelectorLiabilityRegret
	if err := liabilityScope.validate(); err != nil {
		refuse("%v", err)
	} else {
		var err error
		regret, liabilities, err = s.SelectorLiabilityRegretForScope(ctx, liabilityScope)
		if err != nil {
			return CellPromotionEvidence{}, err
		}
	}
	evidence.LiabilityRegret = regret
	evidence.ChallengerSupplierLiability = liabilities[scope.CellID]
	evidence.IncumbentSupplierLiability = liabilities[incumbentCell]
	evidence.UnknownPlatformCostComponents = unresolvedPlatformCostComponents(
		evidence.ChallengerSupplierLiability, evidence.IncumbentSupplierLiability)

	challengerLiability, challengerOK := measuredSupplierLiability(liabilities, scope.CellID)
	incumbentLiability, incumbentOK := measuredSupplierLiability(liabilities, incumbentCell)
	evidence.ChallengerSupplierLiabilityUSDPerVerifiedUnit = challengerLiability
	evidence.IncumbentSupplierLiabilityUSDPerVerifiedUnit = incumbentLiability

	if !challengerOK {
		refuse("challenger %s has no measured supplier-liability proxy in the exact promotion scope on %s/%s (%d of %d samples needed; authority refusals=%v)",
			scope.CellID, scope.HWClass, scope.HardwareIdentity, evidence.ChallengerSupplierLiability.Samples,
			minSupplierLiabilitySamples, evidence.ChallengerSupplierLiability.AuthorityRefusals)
	}
	if !incumbentOK {
		refuse("incumbent %s has no measured supplier-liability proxy in the exact promotion scope on %s/%s (%d of %d samples needed; authority refusals=%v)",
			incumbentCell, scope.HWClass, scope.HardwareIdentity, evidence.IncumbentSupplierLiability.Samples,
			minSupplierLiabilitySamples, evidence.IncumbentSupplierLiability.AuthorityRefusals)
	}
	if evidence.ChallengerSupplierLiability.Samples > 0 &&
		evidence.ChallengerSupplierLiability.RuntimeID != scope.RuntimeID {
		refuse("challenger observations bind runtime %s, promotion scope claims %s",
			evidence.ChallengerSupplierLiability.RuntimeID, scope.RuntimeID)
	}
	for role, observed := range map[string]struct {
		cellID string
		proxy  MeasuredSupplierLiabilityProxy
	}{
		"challenger": {scope.CellID, evidence.ChallengerSupplierLiability},
		"incumbent":  {incumbentCell, evidence.IncumbentSupplierLiability},
	} {
		proxy := observed.proxy
		if proxy.Samples > 0 && (proxy.HWClass != scope.HWClass ||
			proxy.HardwareIdentity != scope.HardwareIdentity) {
			refuse("%s observations bind hardware %s/%s, promotion scope claims %s/%s",
				role, proxy.HWClass, proxy.HardwareIdentity,
				scope.HWClass, scope.HardwareIdentity)
		}
		if proxy.Samples == 0 {
			continue
		}
		if err := validateMeasuredProxyCurrentExecutionIdentity(
			proxy, observed.cellID); err != nil {
			refuse("%s observations cannot bind current execution identity for %s: %v",
				role, observed.cellID, err)
		}
	}
	if incumbentRuntime := promotionRuntimeForCell(activation, scope.JobType, scope.ModelRef, incumbentCell); evidence.IncumbentSupplierLiability.Samples > 0 &&
		evidence.IncumbentSupplierLiability.RuntimeID != incumbentRuntime {
		refuse("incumbent observations bind runtime %s, current authority binds %s",
			evidence.IncumbentSupplierLiability.RuntimeID, incumbentRuntime)
	}
	if evidence.ChallengerSupplierLiability.RetryRate != 0 {
		refuse("challenger %s observed retry rate %g; accepted supplier payout does not price retry burden and no governed reliability threshold authorizes it",
			scope.CellID, evidence.ChallengerSupplierLiability.RetryRate)
	}
	if evidence.ChallengerSupplierLiability.VerificationFails > 0 {
		refuse("challenger %s failed verification %d of %d times; accepted supplier payout does not establish reliability of a rejected outcome",
			scope.CellID, evidence.ChallengerSupplierLiability.VerificationFails, evidence.ChallengerSupplierLiability.VerificationSamples)
	}
	if evidence.ChallengerSupplierLiability.TerminalFails > 0 {
		refuse("challenger %s failed outright on %d of %d terminal attempts; work that never "+
			"produced a result is ineligible, not an extra supplier payment",
			scope.CellID, evidence.ChallengerSupplierLiability.TerminalFails,
			evidence.ChallengerSupplierLiability.TerminalAttempts)
	}
	if evidence.ChallengerSupplierLiability.VerificationSamples == 0 {
		refuse("challenger %s has no verified sample; supplier liability without a verification outcome is unproven",
			scope.CellID)
	}
	if challengerOK && incumbentOK {
		evidence.SupplierLiabilityReductionFraction = (incumbentLiability - challengerLiability) / incumbentLiability
		// Which argument is even available decides which margin applies. A tie on
		// liability is the normal case for two cells serving one model, so treating it
		// as a failed cost argument would refuse every equal-liability promotion on the
		// grounds that it saved nothing — when what it offers is capacity.
		tied := supplierLiabilitiesTieUSD(challengerLiability, incumbentLiability)
		switch {
		case tied:
			evidence.Basis = promotionBasisThroughput
			evidence.RequiredMarginFraction = promotionThroughputMarginFraction
			gain := 0.0
			if in, out := evidence.ChallengerSupplierLiability.MedianMsPerUnit, evidence.IncumbentSupplierLiability.MedianMsPerUnit; in > 0 && out > 0 {
				gain = (out - in) / out
			}
			evidence.ThroughputGainFraction = gain
			if gain < promotionThroughputMarginFraction {
				// Phrased by direction. "only -28.57% faster" is a double negative a
				// reader has to decode, and the two cases are different findings: a
				// slower challenger is a reason to stop looking, a marginally faster
				// one is a reason to gather more samples.
				speed := fmt.Sprintf("only %.2f%% faster, below the %.0f%% throughput margin",
					gain*100, promotionThroughputMarginFraction*100)
				if gain < 0 {
					speed = fmt.Sprintf("%.2f%% SLOWER per unit", -gain*100)
				}
				refuse("challenger %s has equal supplier liability per verified unit (%.2f%% apart) and is %s; an "+
					"equal-liability promotion has to buy capacity and makes no total-cost claim",
					scope.CellID, evidence.SupplierLiabilityReductionFraction*100, speed)
			}
		default:
			evidence.Basis = promotionBasisSupplierLiabilityOnly
			evidence.RequiredMarginFraction = 0
			refuse("cost-based promotion refused: challenger supplier-liability proxy differs by %.2f%%, but platform-cost components remain unknown (%v)",
				evidence.SupplierLiabilityReductionFraction*100, evidence.UnknownPlatformCostComponents)
		}
	}
	// Latency is an independent contract. Unequal supplier liability has already
	// refused the total-cost arm above; for latency-sensitive traffic, a slower
	// challenger adds a second refusal rather than being treated as a trade. The
	// ratio is still output-normalised and therefore must not be reported unless
	// both rows cleared the same exact-geometry gate as the liability comparison.
	if in, out := evidence.ChallengerSupplierLiability.MedianMsPerUnit, evidence.IncumbentSupplierLiability.MedianMsPerUnit; challengerOK && incumbentOK && in > 0 && out > 0 {
		evidence.LatencyRatio = in / out
		if evidence.LatencyRatio > 1 && scopeIsLatencySensitive(scope.LatencyClass) {
			refuse("challenger %s is %.0f%% slower per unit (%.3f ms vs %.3f ms) and %s is a "+
				"latency class where that is the product, not a trade",
				scope.CellID, (evidence.LatencyRatio-1)*100, in, out, scope.LatencyClass)
		}
	}
	if regret.ExactPairScoredDecisions == 0 {
		refuse("no recorded selector decision for %s routed the incumbent and scored the exact incumbent/challenger pair under this policy epoch; a promotion needs production decisions about this pair, not an unrelated decision or benchmark",
			liabilityScope)
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

func promotionRuntimeForCell(
	activation *runtimeActivation, jobType, modelRef, cellID string,
) string {
	for _, profile := range activation.profiles() {
		for _, cell := range profile.Cells {
			if cell.ID == cellID && cell.Job == jobType && cell.Model == modelRef {
				return profile.RuntimeID
			}
		}
	}
	return ""
}

// verifyPromotionScopeAgainstAuthority checks the scope against the runtime
// authority: the challenger must exist, declare the claimed contracts, and be
// reachable by directed routing (that is how it earned measurements at all), and
// the incumbent must be the cell currently routable for this workload.
func verifyPromotionScopeAgainstAuthority(
	activation *runtimeActivation, scope CellPromotionScope, incumbentCell string,
	refuse func(string, ...any),
) error {
	wantRevision := modelRevisionFor(scope.ModelRef)
	if wantRevision == "" {
		refuse("model %s has no declared artifact revision authority", scope.ModelRef)
	} else if scope.ModelRevision != wantRevision {
		refuse("model %s authority binds revision %s, scope claims %s",
			scope.ModelRef, wantRevision, scope.ModelRevision)
	}
	for role, cellID := range map[string]string{
		"challenger": scope.CellID,
		"incumbent":  incumbentCell,
	} {
		_, _, summary, err := currentRuntimeCellBenchmarkIdentity(cellID)
		if err != nil {
			refuse("%s %s lacks current exact benchmark identity: %v", role, cellID, err)
			continue
		}
		if summary.HWClass != scope.HWClass ||
			summary.HardwareIdentity != scope.HardwareIdentity {
			refuse("%s %s current benchmark binds hardware %s/%s, scope claims %s/%s",
				role, cellID, summary.HWClass, summary.HardwareIdentity,
				scope.HWClass, scope.HardwareIdentity)
		}
	}
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
				if !containsString(profile.Hardware.Platforms, scope.HWClass) {
					refuse("challenger %s runtime %s does not declare hardware class %s",
						cell.ID, profile.RuntimeID, scope.HWClass)
				}
				if !cell.ReachableByDirectedRouting(profile) {
					refuse("challenger %s is not reachable by directed routing, so its measurements cannot be attributed to it",
						cell.ID)
				}
			case incumbentCell:
				incumbentFound = true
				if tier := cell.qualityTierFor(profile); tier != scope.QualityTier {
					refuse("incumbent %s sells quality tier %s, scope claims %s",
						cell.ID, tier, scope.QualityTier)
				}
				if cell.Verification != scope.Verification {
					refuse("incumbent %s sells verification %s, scope claims %s",
						cell.ID, cell.Verification, scope.Verification)
				}
				if !containsString(profile.Hardware.Platforms, scope.HWClass) {
					refuse("incumbent %s runtime %s does not declare hardware class %s",
						cell.ID, profile.RuntimeID, scope.HWClass)
				}
				if !activation.cellRoutable(profile, cell) {
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
