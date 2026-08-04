package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
)

// Shadow runtime selection: what a selector WOULD have chosen, recorded beside
// what admission actually froze, changing nothing.
//
// Ordinary admission is a singleton today. Competing engines do not compete for
// production traffic: runtimeCapabilityForBindingDirected freezes exactly one
// advertised cell per (job type, model), and ClaimTasksTx requires
// frozen->>'cell_id' = wac.cell_id. Multi-candidate selection is shadow-only —
// this file. The engine tournament that would make multi-candidate production
// routing real is a separate lane; do not read this scorer as if it already routes.
//
// Two properties of this tree decide the shape:
//
//   - Ordinary admission already requires exactly ONE eligible cell. Scoring
//     over the routable set would record one candidate and one winner on every
//     job, which measures nothing.
//
//   - The routed cell is always the frozen cell. Regret computed over ordinary
//     traffic is identically zero by construction, however elaborate the
//     estimator in front of it.
//
// So the comparison is over the DIRECTED set — every cell an operator or an
// experiment may name, which for embed includes llama.cpp's REAL_RUNTIME_PROVEN
// cell alongside candle's routable one. That is the question a promotion actually
// has to answer: would we have chosen the proven cell if it were allowed to run?
//
// The scoring half arrived. The earlier version of this comment said no per-cell
// source for latency, cost, quality or failure existed in the tree, and that a
// scorer inventing them produces a number whose only property is that it looks
// like evidence. The first half was wrong: every committed task already records
// its cell, its units, its duration, its frozen supplier liability, its retries
// and its verification outcome. runtime_cell_cost.go reads exactly those, and a
// decision with two or more MEASURED candidates on one hardware class is ranked
// on the cost of a verified unit. The second half still holds and is why nothing
// here predicts memory, and why storage, egress, energy, depreciation and refund
// risk are reported as `unknown` rather than as zero.

// shadowSelectionPolicy names the rule that picked the shadow cell. It is stored
// with every row, so a decision taken under one rule is never re-read as though
// it had been taken under another.
//
// v2 because the scoring half arrived. The paragraph above still describes the
// eligibility half exactly; what changed is that runtime_cell_cost.go found the
// per-cell measurement source in the money path, so a decision can now be ranked
// on the measured cost of a VERIFIED unit instead of on the lifecycle ladder
// alone. Which arm actually fired is recorded per row as the selection basis —
// the policy says what rule was available, the basis says what it could prove.
const shadowSelectionPolicy = "eligibility-and-measured-cost-v2"

// Selection bases. A row records exactly one.
const (
	// selectionBasisLadder ranks on the governed lifecycle ladder and quality
	// tier. Used when fewer than two candidates have a measured cost on any one
	// hardware class — which is the honest answer, not a degraded one.
	selectionBasisLadder = "LIFECYCLE_LADDER"
	// selectionBasisMeasuredCost ranks on measured expected verified-outcome cost
	// per unit, on one hardware class. Used only when one candidate is strictly
	// cheaper; a cost-tie must not claim this basis (see throughput / tie bases).
	selectionBasisMeasuredCost = "MEASURED_VERIFIED_OUTCOME_COST"
	// selectionBasisThroughputEqualPrice ranks on measured median latency when
	// verified-outcome costs tie. Capacity gain at equal price — not a saving.
	// Same name as the promotion throughput basis so a receipt cannot re-label
	// a capacity win as a cost win by switching vocabularies.
	selectionBasisThroughputEqualPrice = "MORE_THROUGHPUT_AT_EQUAL_PRICE"
	// selectionBasisTieNoDecision records that every term the projection binds
	// also tied (cost, reliability, and latency within noise). Preferring the
	// routed cell is not a judgement and must not be scored as one.
	selectionBasisTieNoDecision = "TIE_NO_DECISION"
)

// latencyNoiseFraction is how close two median_ms_per_unit values must be to
// count as a latency tie for shadow selection. Two percent: tighter than the
// 25% promotion throughput margin (which refuses a weak capacity claim) and
// wide enough that sub-millisecond timer jitter on a shared host is not a
// manufactured winner. Absolute floor is applied in latenciesTie.
const latencyNoiseFraction = 0.02

// latencyNoiseAbsMs is the absolute floor under which two latencies are treated
// as equal regardless of ratio. A 0.01 ms gap on a 0.2 ms unit is a large ratio
// and a meaningless absolute difference on this host.
const latencyNoiseAbsMs = 0.01

// shadowCandidate is one cell the selector considered.
type shadowCandidate struct {
	CellID    string `json:"cell_id"`
	RuntimeID string `json:"runtime_id"`
	Engine    string `json:"engine"`
	ModelKind string `json:"model_kind"`
	Lifecycle string `json:"lifecycle"`
	Routable  bool   `json:"routable"`
	// QualityTier and Verification are the quality and determinism contracts the
	// cell sells. A selection that ignored them would be choosing on speed.
	QualityTier  string `json:"quality_tier"`
	Verification string `json:"verification"`

	// The measured half, present only when this cell cleared minCellCostSamples
	// on the hardware class the decision was scored on. Absent fields mean "not
	// measured", which is why they are omitempty rather than zero: a zero cost
	// would read as free.
	CostMeasured       bool    `json:"cost_measured"`
	CostSamples        int     `json:"cost_samples,omitempty"`
	VerifiedUSDPerUnit float64 `json:"verified_outcome_usd_per_unit,omitempty"`
	// MedianMsPerUnit is carried when measured so a cost-tie can be re-ranked on
	// throughput without a second query. Zero with CostMeasured=true is a real
	// measurement (free-as-in-instant), not an absent field — so no omitempty.
	MedianMsPerUnit float64 `json:"median_ms_per_unit,omitempty"`
}

// shadowExclusion is a cell that was rejected, and why in the cell's own terms.
//
// The excluded set is the half that makes a selection reviewable. "It chose the
// candle cell" is not something anyone can check; "it excluded the llama.cpp
// generation cell because that cell sells byte_exact and its measured engine is
// not byte-deterministic" is.
type shadowExclusion struct {
	CellID string `json:"cell_id"`
	Reason string `json:"reason"`
}

// ShadowSelection is one recorded decision.
type ShadowSelection struct {
	JobID            string            `json:"job_id"`
	RuntimeMatrixSHA string            `json:"runtime_matrix_sha256"`
	PolicyRevision   int64             `json:"policy_revision"`
	JobType          string            `json:"job_type"`
	ModelRef         string            `json:"model_ref"`
	ModelKind        string            `json:"model_kind"`
	WorkloadClass    string            `json:"workload_class"`
	LatencyClass     string            `json:"latency_class"`
	RoutedCellID     string            `json:"routed_cell_id"`
	ShadowCellID     string            `json:"shadow_cell_id"`
	Considered       []shadowCandidate `json:"considered_cells"`
	Excluded         []shadowExclusion `json:"excluded_cells"`
	SelectionPolicy  string            `json:"selection_policy"`
	// SelectionBasis is which arm of the policy decided this row, and CostHWClass
	// is the single hardware class the cost comparison was made on. Empty when the
	// basis is the ladder, because there was no comparison.
	SelectionBasis string `json:"selection_basis"`
	CostHWClass    string `json:"cost_hw_class"`
	// ExecutionMode is where the workload was placed and why. Empty when the
	// placement rule refused every mode, which is a state worth being able to see
	// rather than one to paper over with a default.
	ExecutionMode       string `json:"execution_mode"`
	ExecutionModeReason string `json:"execution_mode_reason"`
	// TopologyPlan is retained even when placement is refused. A refusal is
	// evidence about the requested parallel shape and measured fabric, not an
	// empty result that could later be mistaken for a default.
	Topology TopologyPlan `json:"topology_plan"`
}

// withExecutionMode records the placement decision for this workload.
//
// Batch work reaches POOL today by construction — admission freezes one cell and
// the scheduler fans tasks out with no inter-worker communication — and that is
// exactly why the decision is worth storing: once a second mode is reachable,
// "by construction" and "by decision" stop being the same thing, and only a
// stored reason distinguishes them afterwards.
//
// The fabric is reported as UNKNOWN because nothing in this tree measures link
// bandwidth or latency between workers. That is not a placeholder: an unmeasured
// fabric is precisely what ChooseExecutionMode refuses to place tightly coupled
// work on, so passing UNKNOWN is the honest input and the refusal it produces is
// the correct answer.
func (s ShadowSelection) withExecutionMode(decision WorkloadDecision) ShadowSelection {
	topology, err := PlanTopology(TopologyRequest{
		WorkloadClass: decision.WorkloadClass,
		Parallelism:   decision.Parallelism,
		Fabric:        FabricUnknown,
		// A job that reached this point was admitted against a frozen runtime
		// candidate, so community capacity for it existed. The deadline is not at
		// risk at submit time — nothing has run yet — and a cloud backstop is not
		// asserted, because no buyer term in this tree says a provider is allowed.
		CommunityCapacityAvailable: true,
		CloudBackstopPermitted:     false,
	})
	if err != nil {
		// Malformed parallelism is a refusal too, but preserve the prior contract:
		// no execution mode is recorded as though an invalid request had placed.
		s.Topology = TopologyPlan{Version: topologyPlanVersion, Status: "REFUSED",
			Parallelism: decision.Parallelism.Mode, Degree: decision.Parallelism.TensorParallelDegree,
			Fabric: FabricUnknown, Reason: err.Error()}
		return s
	}
	s.Topology = topology
	if topology.Status != "ACCEPTED" {
		return s
	}
	s.ExecutionMode = string(topology.PlacementMode)
	// Include the topology shape in the persisted explanation so the mode cannot
	// be read as a generic POOL/cluster label without its workload semantics.
	s.ExecutionModeReason = topology.Explain()
	return s
}

// Diverged reports whether the shadow would have chosen differently. This is the
// only interesting bit on the row, and the partial index is built on it.
func (s ShadowSelection) Diverged() bool { return s.ShadowCellID != s.RoutedCellID }

// planShadowSelection scores the directed set for a frozen workload decision.
//
// Pure: it reads the authority and the frozen decision and touches nothing else.
// It cannot reach the scheduler, admission, pricing or any money path, which is
// what keeps a shadow decision structurally incapable of becoming a real one.
func planShadowSelection(decision WorkloadDecision) (ShadowSelection, error) {
	if len(decision.RuntimeCandidates) == 0 {
		return ShadowSelection{}, fmt.Errorf("workload decision froze no runtime candidate")
	}
	routed := decision.RuntimeCandidates[0]
	activation := currentActivation()

	out := ShadowSelection{
		RuntimeMatrixSHA: generatedRuntimeMatrixSHA256,
		PolicyRevision:   activation.PolicyRevision,
		JobType:          decision.Binding.JobType.Type,
		ModelRef:         decision.Binding.Model.Ref,
		ModelKind:        decision.Binding.Model.Kind,
		WorkloadClass:    decision.WorkloadClass,
		LatencyClass:     decision.LatencyClass,
		RoutedCellID:     routed.CellID,
		SelectionPolicy:  shadowSelectionPolicy,
		SelectionBasis:   selectionBasisLadder,
		Considered:       []shadowCandidate{},
		Excluded:         []shadowExclusion{},
	}

	// The DIRECTED set, not the advertised one. Ordinary admission has already
	// collapsed the advertised set to a singleton by the time this runs, so
	// scoring it again would record the answer it was handed.
	for _, profile := range activation.profiles() {
		for _, cell := range profile.Cells {
			if cell.Job != decision.Binding.JobType.Type || cell.Model != decision.Binding.Model.Ref {
				continue
			}
			lifecycle := cell.EffectiveLifecycle(profile)
			if !cell.ReachableByDirectedRouting(profile) {
				reason := fmt.Sprintf("lifecycle %s is not reachable by any route", lifecycle)
				if lifecycle == runtimeLifecycleRejectedForContract {
					// Say what measurement decided, not merely that it was
					// excluded. A rejection with a stated reason is the only kind
					// a reader can argue with.
					reason = fmt.Sprintf("REJECTED_FOR_CONTRACT: %s", cell.RejectionReason)
				}
				out.Excluded = append(out.Excluded, shadowExclusion{CellID: cell.ID, Reason: reason})
				continue
			}
			if float64(decision.MinimumMemoryGB) < cell.MinMemoryGB {
				out.Excluded = append(out.Excluded, shadowExclusion{
					CellID: cell.ID,
					Reason: fmt.Sprintf("cell needs %.3f GB, the frozen placement floor is %.3f GB",
						cell.MinMemoryGB, decision.MinimumMemoryGB),
				})
				continue
			}
			if cell.benchmarkAuthorityFor(profile) == "" {
				out.Excluded = append(out.Excluded, shadowExclusion{
					CellID: cell.ID, Reason: "no benchmark authority",
				})
				continue
			}
			model := runtimeAuthorityModels[cell.Model]
			out.Considered = append(out.Considered, shadowCandidate{
				CellID: cell.ID, RuntimeID: profile.RuntimeID, Engine: profile.Engine,
				ModelKind: wireKindFor(cell, model.WireKind), Lifecycle: lifecycle,
				Routable:     cell.Routable(profile),
				QualityTier:  cell.qualityTierFor(profile),
				Verification: cell.Verification,
			})
		}
	}
	sort.Slice(out.Considered, func(i, j int) bool {
		return out.Considered[i].CellID < out.Considered[j].CellID
	})
	sort.Slice(out.Excluded, func(i, j int) bool {
		return out.Excluded[i].CellID < out.Excluded[j].CellID
	})

	out.ShadowCellID = chooseShadowCell(out.Considered, routed.CellID)
	if out.ShadowCellID == "" {
		return ShadowSelection{}, fmt.Errorf(
			"no cell survived eligibility for job_type=%q model=%q, yet admission froze %q",
			out.JobType, out.ModelRef, routed.CellID)
	}
	return out, nil
}

// rankedByMeasuredCost re-decides the shadow cell on measured economics when —
// and only when — two or more candidates have a measured cost on one hardware
// class.
//
// Ranking honesty:
//
//   - When one cell is strictly cheaper on verified-outcome USD/unit, the basis
//     is MEASURED_VERIFIED_OUTCOME_COST and that cell wins.
//   - When costs tie (the common case for same-model catalogue pricing, where
//     duration cancels from supplier entitlement), the basis is
//     MORE_THROUGHPUT_AT_EQUAL_PRICE and the faster cell wins. Claiming a cost
//     win on a cost-tie is false.
//   - When cost and latency both tie, the basis is TIE_NO_DECISION and the
//     routed cell is kept. A correct refusal to choose is a real result.
//
// Separate from planShadowSelection, and applied after it, for two reasons. The
// planner stays pure and database-free, so it remains testable without Postgres;
// and the cost query only runs for a decision that actually had a choice to make,
// which is what keeps it off the hot path of the single-candidate submits that
// make up ordinary traffic.
//
// ponytail: the cost query aggregates `tasks` joined to `jobs` with no index on
// (job_type, model_ref). At a few thousand tasks that is nothing; if the table
// grows and this shows up in submit latency, the fix is a partial index on
// completed primary tasks, not a cache.
func (s ShadowSelection) rankedByMeasuredCost(
	byHW map[string]map[string]MeasuredCellCost,
) ShadowSelection {
	cells := make([]string, 0, len(s.Considered))
	for _, candidate := range s.Considered {
		cells = append(cells, candidate.CellID)
	}
	hw := comparableHardwareFor(byHW, cells)
	if hw == "" {
		return s
	}
	costs := byHW[hw]
	for i, candidate := range s.Considered {
		cost, ok := measuredCost(costs, candidate.CellID)
		if !ok {
			continue
		}
		s.Considered[i].CostMeasured = true
		s.Considered[i].CostSamples = costs[candidate.CellID].Samples
		s.Considered[i].VerifiedUSDPerUnit = cost
		s.Considered[i].MedianMsPerUnit = costs[candidate.CellID].MedianMsPerUnit
	}
	decision := decideMeasuredShadow(costs, cells, s.RoutedCellID)
	if decision.Basis == "" || decision.Winner == "" {
		return s
	}
	s.ShadowCellID = decision.Winner
	s.SelectionBasis = decision.Basis
	s.CostHWClass = hw
	return s
}

// measuredShadowDecision is the pure outcome of ranking measured candidates.
type measuredShadowDecision struct {
	Winner string
	Basis  string
}

// decideMeasuredShadow picks a winner and names the term that actually decided.
// Pure: no DB, no admission side effects. Used by rankedByMeasuredCost and by
// the governed comparison receipt so both surfaces cannot disagree on basis.
func decideMeasuredShadow(
	costs map[string]MeasuredCellCost, cells []string, routed string,
) measuredShadowDecision {
	ranked := rankCellsByMeasuredCost(costs, cells)
	if len(ranked) < 2 {
		return measuredShadowDecision{}
	}
	// Strict cost win: cheapest is meaningfully below second.
	bestCost, okBest := measuredCost(costs, ranked[0])
	secondCost, okSecond := measuredCost(costs, ranked[1])
	if !okBest || !okSecond {
		return measuredShadowDecision{}
	}
	if !costsTieUSD(bestCost, secondCost) {
		return measuredShadowDecision{Winner: ranked[0], Basis: selectionBasisMeasuredCost}
	}
	// Cost tie. Rank on throughput (lower median ms/unit is more capacity).
	byLatency := rankCellsByMeasuredLatency(costs, ranked)
	if len(byLatency) < 2 {
		// Only one cell has a positive latency sample among the cost-tied set.
		if len(byLatency) == 1 {
			return measuredShadowDecision{
				Winner: byLatency[0], Basis: selectionBasisThroughputEqualPrice,
			}
		}
		return measuredShadowDecision{Winner: routed, Basis: selectionBasisTieNoDecision}
	}
	fast := costs[byLatency[0]].MedianMsPerUnit
	slow := costs[byLatency[1]].MedianMsPerUnit
	if latenciesTie(fast, slow) {
		// Every ranking term tied. Prefer the routed cell so sort order does not
		// manufacture a divergence, and name the refusal.
		winner := routed
		if winner == "" {
			winner = byLatency[0]
		}
		// If the routed cell is not among the measured set, keep the first.
		if _, ok := costs[winner]; !ok || !costs[winner].Measured {
			winner = byLatency[0]
		}
		return measuredShadowDecision{Winner: winner, Basis: selectionBasisTieNoDecision}
	}
	return measuredShadowDecision{
		Winner: byLatency[0], Basis: selectionBasisThroughputEqualPrice,
	}
}

// costsTieUSD reports whether two verified-outcome costs are the same number
// for selection purposes (same band as promotion pricesTieWithin).
func costsTieUSD(a, b float64) bool {
	if a <= 0 || b <= 0 {
		return false
	}
	mid := (a + b) / 2
	if mid <= 0 {
		return false
	}
	return math.Abs(a-b)/mid < pricesTieWithin
}

// latenciesTie reports whether two median_ms_per_unit values are indistinguishable
// for selection. Absolute floor first: sub-centisecond gaps on this host are
// noise, not capacity.
func latenciesTie(a, b float64) bool {
	if a <= 0 || b <= 0 {
		return true // unusable latency is not a ranking signal
	}
	if math.Abs(a-b) < latencyNoiseAbsMs {
		return true
	}
	mid := (a + b) / 2
	return math.Abs(a-b)/mid < latencyNoiseFraction
}

// rankCellsByMeasuredLatency orders measured cells fastest-first (lowest
// median_ms_per_unit). Unmeasured or non-positive latency cells are dropped.
// Ties break toward the cell id so the order is stable; callers that want the
// routed cell to win a true latency-tie use decideMeasuredShadow instead.
func rankCellsByMeasuredLatency(costs map[string]MeasuredCellCost, cells []string) []string {
	type scored struct {
		cell string
		ms   float64
	}
	var ranked []scored
	for _, cell := range cells {
		c, ok := costs[cell]
		if !ok || !c.Measured || c.MedianMsPerUnit <= 0 {
			continue
		}
		ranked = append(ranked, scored{cell, c.MedianMsPerUnit})
	}
	sort.Slice(ranked, func(i, j int) bool {
		if ranked[i].ms != ranked[j].ms {
			return ranked[i].ms < ranked[j].ms
		}
		return ranked[i].cell < ranked[j].cell
	})
	out := make([]string, 0, len(ranked))
	for _, r := range ranked {
		out = append(out, r.cell)
	}
	return out
}

// chooseShadowCell applies the ranking available before any cell was measured.
//
// There is no cost model yet, so ranking by predicted cost would be ranking by a
// number nothing measured. What IS governed is the lifecycle ladder and the
// quality tier, so the rule is: prefer the most proven cell, and break ties
// toward the one admission actually chose.
//
// That last clause matters. Without it a tie would pick alphabetically and half
// the rows would record a divergence that reflects sort order rather than a
// judgement, which is exactly the kind of number that looks like evidence.
func chooseShadowCell(considered []shadowCandidate, routed string) string {
	best, bestRank := "", -1
	for _, candidate := range considered {
		rank, known := cellLifecycleRank(candidate.Lifecycle)
		if !known {
			continue
		}
		if candidate.QualityTier == "OUTCOME_EQUIVALENT" {
			rank++ // a measured equivalence outranks an unproven cell at the same state
		}
		switch {
		case rank > bestRank:
			best, bestRank = candidate.CellID, rank
		case rank == bestRank && candidate.CellID == routed:
			best = candidate.CellID
		}
	}
	return best
}

// RecordShadowSelection persists one decision. Called AFTER the job has been
// committed, and its error is for the caller to log and drop: a selector that
// could refuse a submit would be a router.
func (s *Store) RecordShadowSelection(ctx context.Context, jobID string, sel ShadowSelection) error {
	considered, err := json.Marshal(sel.Considered)
	if err != nil {
		return err
	}
	excluded, err := json.Marshal(sel.Excluded)
	if err != nil {
		return err
	}
	topology, err := json.Marshal(sel.Topology)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `
		INSERT INTO runtime_shadow_selections
		  (job_id, runtime_matrix_sha256, policy_revision, job_type, model_ref,
		   model_kind, workload_class, latency_class, routed_cell_id, shadow_cell_id,
		   considered_cells, excluded_cells, selection_policy, selection_basis,
		   cost_hw_class, execution_mode, execution_mode_reason, topology_plan)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18::jsonb)
		ON CONFLICT (job_id) DO NOTHING`,
		jobID, sel.RuntimeMatrixSHA, sel.PolicyRevision, sel.JobType, sel.ModelRef,
		sel.ModelKind, sel.WorkloadClass, sel.LatencyClass, sel.RoutedCellID,
		sel.ShadowCellID, string(considered), string(excluded), sel.SelectionPolicy,
		sel.SelectionBasis, sel.CostHWClass, sel.ExecutionMode, sel.ExecutionModeReason, string(topology))
	return err
}

// ShadowSelectionDivergence is the read side: how often the shadow disagreed.
//
// Reported as counts rather than as a rate, because a rate over a handful of jobs
// reads as a measurement and is not one.
type ShadowSelectionDivergence struct {
	JobType   string `json:"job_type"`
	ModelRef  string `json:"model_ref"`
	Decisions int    `json:"decisions"`
	Diverged  int    `json:"diverged"`
}

func (s *Store) ShadowSelectionDivergence(ctx context.Context) ([]ShadowSelectionDivergence, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT job_type, model_ref, COUNT(*),
		       COUNT(*) FILTER (WHERE shadow_cell_id <> routed_cell_id)
		  FROM runtime_shadow_selections
		 GROUP BY job_type, model_ref
		 ORDER BY job_type, model_ref`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ShadowSelectionDivergence
	for rows.Next() {
		var row ShadowSelectionDivergence
		if err := rows.Scan(&row.JobType, &row.ModelRef, &row.Decisions, &row.Diverged); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}
