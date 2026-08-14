package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"time"
)

// Offline Mode A replay regret (Network V2 Step 21 / G021–G053).
//
// Re-scores stored runtime_shadow_selections under a named alternative policy
// without writing, promoting, or influencing live selection. Mode B (full
// offer-book / claim-time fleet counterfactuals) is refused with named missing
// fields rather than fabricated.
//
// Policy implemented here:
//
//	lifecycle-ladder-v1 — re-rank considered cells by lifecycle rank + quality
//	tier only (chooseShadowCell). Compares the stored routed cell against what
//	that rule would have picked from the same frozen candidate set.
//
// This is deliberately the same ranking production's shadow path already uses
// when supplier-liability is unavailable, so the report measures divergence of
// ordinary routed traffic from a pure ladder policy, not a new invention.

const (
	replayPolicyLifecycleLadder = "lifecycle-ladder-v1"
	replayPolicyStoredShadow    = "stored-shadow-cell-v1"
)

// ReplayRegretReport is the evidence artifact body for Mode A offline replay.
type ReplayRegretReport struct {
	SchemaVersion int    `json:"schema_version"`
	Kind          string `json:"kind"`
	Status        string `json:"status"`
	SourceCommit  string `json:"source_commit"`
	GeneratedAt   string `json:"generated_at"`

	// Policy is the alternative selection rule applied offline.
	Policy         string `json:"policy"`
	BaselinePolicy string `json:"baseline_policy"`
	// Baseline is what production recorded as routed_cell_id.

	Decisions         int `json:"decisions"`
	SelectionsChanged int `json:"selections_changed"`
	// UnchangedSelections is decisions where the alternative picked the same
	// cell as the stored routed choice.
	UnchangedSelections int `json:"unchanged_selections"`

	// LatencyRegretMS is selection latency regret: routed median_ms_per_unit −
	// alternative median_ms_per_unit, summed only when both cells carried a
	// measured median on the shadow candidate row. Unknown stays out.
	LatencyRegret LatencyRegretSummary `json:"latency_regret"`
	// CostRegret is supplier-liability regret under the same rule
	// (routed − alternative), using candidate-row supplier liability proxies.
	// Not total platform cost.
	CostRegret CostRegretSummary `json:"cost_regret"`

	// SLARegret and LocalityRegret are refused until the corpus freezes the
	// inputs those metrics need. Honest absence, not zero.
	SLARegret      RegretRefusal `json:"sla_regret"`
	LocalityRegret RegretRefusal `json:"locality_regret"`

	// PhasePredictionCoverage reports how many eta_calibration phase rows
	// exist alongside totals — the G053 surface, observation-only.
	PhasePredictionCoverage PhaseCoverageSummary `json:"phase_prediction_coverage"`

	// ModeBRefusal names what faithful full-network replay still lacks.
	ModeBRefusal RegretRefusal `json:"mode_b_refusal"`

	// HonestRefusals collects every metric this report declined to invent.
	HonestRefusals []string `json:"honest_refusals"`

	// MoneyAuthority confirms this report cannot influence selection.
	MoneyAuthority string `json:"money_authority"`
}

type LatencyRegretSummary struct {
	Scored               int     `json:"scored_decisions"`
	Unmeasured           int     `json:"unmeasured_decisions"`
	TotalRegretMSPerUnit float64 `json:"total_regret_ms_per_unit"`
	MeanRegretMSPerUnit  float64 `json:"mean_regret_ms_per_unit"`
	// Positive means the routed cell was slower than the alternative.
}

type CostRegretSummary struct {
	Scored                      int     `json:"scored_decisions"`
	Unmeasured                  int     `json:"unmeasured_decisions"`
	TotalLiabilityRegretPerUnit float64 `json:"total_supplier_liability_regret_per_verified_unit"`
	MeanLiabilityRegretPerUnit  float64 `json:"mean_supplier_liability_regret_per_verified_unit"`
}

type RegretRefusal struct {
	Status string `json:"status"` // REFUSED
	Why    string `json:"why"`
}

type PhaseCoverageSummary struct {
	TotalPhaseRows     int            `json:"total_phase_rows"`
	ByPhase            map[string]int `json:"by_phase"`
	RowsWithPrediction int            `json:"rows_with_prediction"`
	RowsActualOnly     int            `json:"rows_actual_only"`
	Note               string         `json:"note"`
}

// ReplayShadowRegretAgainstPolicy re-scores stored shadow decisions under
// alternativePolicy. It is read-only.
func (s *Store) ReplayShadowRegretAgainstPolicy(
	ctx context.Context, alternativePolicy string,
) (ReplayRegretReport, error) {
	report := ReplayRegretReport{
		SchemaVersion:  1,
		Kind:           "g021_g053_offline_replay_regret",
		Status:         "PARTIAL",
		GeneratedAt:    time.Now().UTC().Format(time.RFC3339),
		Policy:         alternativePolicy,
		BaselinePolicy: "production_routed_cell",
		MoneyAuthority: "OBSERVATION_ONLY_NO_SELECTION_AUTHORITY",
		SLARegret: RegretRefusal{
			Status: "REFUSED",
			Why: "SLA regret needs frozen per-decision SLA class and deadline outcome; " +
				"runtime_shadow_selections does not store either",
		},
		LocalityRegret: RegretRefusal{
			Status: "REFUSED",
			Why: "locality regret needs frozen region/placement alternatives per decision; " +
				"shadow rows freeze cell ids, not region-distance or placement counterfactuals",
		},
		ModeBRefusal: RegretRefusal{
			Status: "REFUSED",
			Why: "Mode B needs claim-time fleet eligibility snapshots (batch), full " +
				"offer-book freezes (realtime/service), network epochs, and matched " +
				"incumbent/challenger execution pairs — none are captured",
		},
		HonestRefusals: []string{
			"sla_regret: no frozen SLA class/deadline outcome on shadow rows",
			"locality_regret: no frozen region/placement counterfactual on shadow rows",
			"mode_b: full network counterfactual substrate absent",
			"energy_regret: no authoritative energy actuals in this corpus",
			"total_platform_cost_regret: platform components remain unknown (see SelectorLiabilityRegret)",
		},
		PhasePredictionCoverage: PhaseCoverageSummary{
			ByPhase: map[string]int{},
			Note: "eta_calibration phase rows are observation-only; predicted_ms is NULL " +
				"until a per-phase estimator exists. Absence of prediction is not zero regret.",
		},
	}

	switch alternativePolicy {
	case replayPolicyLifecycleLadder, replayPolicyStoredShadow:
	default:
		return report, fmt.Errorf("unknown replay policy %q", alternativePolicy)
	}

	rows, err := s.pool.Query(ctx, `
		SELECT rs.routed_cell_id, rs.shadow_cell_id, rs.considered_cells,
		       rs.selection_policy
		  FROM runtime_shadow_selections rs
		 ORDER BY rs.decided_at`)
	if err != nil {
		return report, err
	}
	defer rows.Close()

	var latTotal, costTotal float64
	for rows.Next() {
		var routed, shadow, policy string
		var candidatesJSON []byte
		if err := rows.Scan(&routed, &shadow, &candidatesJSON, &policy); err != nil {
			return report, err
		}
		var candidates []shadowCandidate
		if err := json.Unmarshal(candidatesJSON, &candidates); err != nil {
			return report, fmt.Errorf("decode shadow candidates: %w", err)
		}
		report.Decisions++

		alt := routed
		switch alternativePolicy {
		case replayPolicyLifecycleLadder:
			alt = chooseShadowCell(candidates, routed)
			if alt == "" {
				alt = routed
			}
		case replayPolicyStoredShadow:
			alt = shadow
			if alt == "" {
				alt = routed
			}
		}
		if alt != routed {
			report.SelectionsChanged++
		} else {
			report.UnchangedSelections++
		}

		routedCand, routedOK := findCandidate(candidates, routed)
		altCand, altOK := findCandidate(candidates, alt)

		// Latency: need measured medians on both.
		if routedOK && altOK &&
			routedCand.SupplierLiabilityMeasured && altCand.SupplierLiabilityMeasured {
			// MedianMsPerUnit is present when measured (see shadowCandidate).
			dLat := routedCand.MedianMsPerUnit - altCand.MedianMsPerUnit
			if !math.IsNaN(dLat) && !math.IsInf(dLat, 0) {
				report.LatencyRegret.Scored++
				latTotal += dLat
			} else {
				report.LatencyRegret.Unmeasured++
			}
		} else {
			report.LatencyRegret.Unmeasured++
		}

		// Cost: supplier liability proxy on both.
		if routedOK && altOK &&
			routedCand.SupplierLiabilityMeasured && altCand.SupplierLiabilityMeasured {
			dCost := routedCand.SupplierLiabilityUSDPerVerifiedUnit -
				altCand.SupplierLiabilityUSDPerVerifiedUnit
			if !math.IsNaN(dCost) && !math.IsInf(dCost, 0) {
				report.CostRegret.Scored++
				costTotal += dCost
			} else {
				report.CostRegret.Unmeasured++
			}
		} else {
			report.CostRegret.Unmeasured++
		}
	}
	if err := rows.Err(); err != nil {
		return report, err
	}
	report.LatencyRegret.TotalRegretMSPerUnit = latTotal
	report.CostRegret.TotalLiabilityRegretPerUnit = costTotal
	if report.LatencyRegret.Scored > 0 {
		report.LatencyRegret.MeanRegretMSPerUnit = latTotal / float64(report.LatencyRegret.Scored)
	}
	if report.CostRegret.Scored > 0 {
		report.CostRegret.MeanLiabilityRegretPerUnit = costTotal / float64(report.CostRegret.Scored)
	}

	// Phase coverage from eta_calibration (G053 surface).
	if err := s.fillPhaseCoverage(ctx, &report.PhasePredictionCoverage); err != nil {
		return report, err
	}

	if report.Decisions == 0 {
		report.Status = "NO_DECISIONS"
		report.HonestRefusals = append(report.HonestRefusals,
			"no runtime_shadow_selections rows in this database — numbers are coverage zeros, not evidence of zero regret")
	}
	return report, nil
}

func findCandidate(cands []shadowCandidate, id string) (shadowCandidate, bool) {
	for _, c := range cands {
		if c.CellID == id {
			return c, true
		}
	}
	return shadowCandidate{}, false
}

func (s *Store) fillPhaseCoverage(ctx context.Context, cov *PhaseCoverageSummary) error {
	rows, err := s.pool.Query(ctx, `
		SELECT phase,
		       COUNT(*)::int,
		       COUNT(*) FILTER (WHERE predicted_ms IS NOT NULL)::int,
		       COUNT(*) FILTER (WHERE predicted_ms IS NULL AND realized_ms IS NOT NULL)::int
		  FROM eta_calibration
		 WHERE COALESCE(phase,'total') <> 'total'
		 GROUP BY phase
		 ORDER BY phase`)
	if err != nil {
		return err
	}
	defer rows.Close()
	cov.ByPhase = map[string]int{}
	for rows.Next() {
		var phase string
		var n, withPred, actualOnly int
		if err := rows.Scan(&phase, &n, &withPred, &actualOnly); err != nil {
			return err
		}
		cov.ByPhase[phase] = n
		cov.TotalPhaseRows += n
		cov.RowsWithPrediction += withPred
		cov.RowsActualOnly += actualOnly
	}
	if cov.ByPhase == nil {
		cov.ByPhase = map[string]int{}
	}
	// Stable empty map for JSON.
	if len(cov.ByPhase) == 0 {
		cov.ByPhase = map[string]int{}
	}
	return rows.Err()
}

// BuildReplayRegretArtifact runs Mode A against the lifecycle-ladder alternative
// and returns a complete report suitable for evidence/.
func (s *Store) BuildReplayRegretArtifact(ctx context.Context, sourceCommit string) (ReplayRegretReport, error) {
	report, err := s.ReplayShadowRegretAgainstPolicy(ctx, replayPolicyLifecycleLadder)
	if err != nil {
		return report, err
	}
	report.SourceCommit = sourceCommit
	// Also score stored-shadow divergence counts into notes via a second pass
	// only for selections_changed cross-check — not a second authority.
	shadowReport, err := s.ReplayShadowRegretAgainstPolicy(ctx, replayPolicyStoredShadow)
	if err == nil {
		report.HonestRefusals = append(report.HonestRefusals, fmt.Sprintf(
			"cross_check stored-shadow-cell-v1: decisions=%d selections_changed=%d (shadow vs routed; not used as promotion evidence)",
			shadowReport.Decisions, shadowReport.SelectionsChanged))
	}
	// Sort refusals for stable JSON.
	sort.Strings(report.HonestRefusals)
	return report, nil
}
