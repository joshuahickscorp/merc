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
// and its verification outcome. runtime_cell_cost.go reads exactly those into a
// measured supplier-liability proxy. It is not a complete cost: storage, egress,
// energy, depreciation and refund risk remain unknown. Consequently it may
// establish equal liability for a throughput comparison, but an unequal proxy
// cannot authorize a cost-based shadow choice.

// shadowSelectionPolicy names the rule that picked the shadow cell. It is stored
// with every row, so a decision taken under one rule is never re-read as though
// it had been taken under another.
//
// v3 makes the authority boundary explicit: the measured money-path value is a
// supplier-liability proxy, not total cost. Which arm actually fired is recorded
// per row, and an unavailable cost arm records its refusal separately.
const shadowSelectionPolicy = "eligibility-and-measured-supplier-liability-v3"

// Selection bases. A row records exactly one.
const (
	// selectionBasisLadder ranks on the governed lifecycle ladder and quality
	// tier. It remains the basis when an unequal supplier-liability proxy cannot
	// authorize a cost decision because platform costs are unknown.
	selectionBasisLadder = "LIFECYCLE_LADDER"
	// selectionBasisThroughputEqualLiability ranks on measured median latency
	// when the supplier-liability proxies tie. Capacity gain at equal supplier
	// liability — not a total-cost saving.
	// Same name as the promotion throughput basis so a receipt cannot re-label
	// a capacity win as a cost win by switching vocabularies.
	selectionBasisThroughputEqualLiability = "MORE_THROUGHPUT_AT_EQUAL_SUPPLIER_LIABILITY"
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

	// The measured half, present only when this cell cleared
	// minSupplierLiabilitySamples
	// on the hardware class the decision was scored on. Absent fields mean "not
	// measured", which is why they are omitempty rather than zero: a zero cost
	// would read as free.
	SupplierLiabilityMeasured           bool    `json:"supplier_liability_proxy_measured"`
	SupplierLiabilitySamples            int     `json:"supplier_liability_proxy_samples,omitempty"`
	SupplierLiabilityUSDPerVerifiedUnit float64 `json:"supplier_liability_proxy_usd_per_verified_unit,omitempty"`
	// MedianMsPerUnit is carried when measured so an equal-liability pair can be ranked on
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
	// SelectionBasis is which arm of the policy decided this row.
	SelectionBasis string `json:"selection_basis"`
	// SupplierLiabilityHWClass is the single hardware class on which the proxies
	// were measured. It may be present with a ladder basis when the cost arm
	// explicitly refused an unequal-liability comparison.
	SupplierLiabilityHWClass string `json:"supplier_liability_hw_class"`
	// SupplierLiabilityHardwareIdentity is the exact device generation shared by
	// every measured proxy. The broad class alone cannot distinguish M1 Ultra
	// observations from an M3 Ultra placement.
	SupplierLiabilityHardwareIdentity string `json:"supplier_liability_hardware_identity"`
	// EconomicsRefusal states why measured supplier liability could not become a
	// cost decision. Empty for ladder-only, equal-liability throughput, and true
	// ties.
	EconomicsRefusal string `json:"economics_refusal,omitempty"`
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

// rankedByMeasuredSupplierLiability re-decides the shadow cell only for the
// honest equal-liability throughput arm. Unequal liabilities cannot establish a
// cost winner while platform-cost components are unknown, so the existing
// lifecycle choice is retained and an explicit refusal is persisted.
//
// Ranking honesty:
//
//   - When supplier-liability proxies differ, cost selection refuses and the
//     lifecycle choice remains.
//   - When liabilities tie (the common case for same-model catalogue pricing, where
//     duration cancels from supplier entitlement), the basis is
//     MORE_THROUGHPUT_AT_EQUAL_SUPPLIER_LIABILITY and the faster cell wins.
//   - When liability and latency both tie, the basis is TIE_NO_DECISION and the
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
func (s ShadowSelection) rankedByMeasuredSupplierLiability(
	byHW map[string]map[string]MeasuredSupplierLiabilityProxy,
) ShadowSelection {
	cells := make([]string, 0, len(s.Considered))
	for _, candidate := range s.Considered {
		cells = append(cells, candidate.CellID)
	}
	hw := comparableHardwareFor(byHW, cells)
	if hw == "" {
		return s
	}
	liabilities := byHW[hw]
	hardwareIdentity := ""
	for i, candidate := range s.Considered {
		liability, ok := measuredSupplierLiability(liabilities, candidate.CellID)
		if !ok {
			continue
		}
		s.Considered[i].SupplierLiabilityMeasured = true
		s.Considered[i].SupplierLiabilitySamples = liabilities[candidate.CellID].Samples
		s.Considered[i].SupplierLiabilityUSDPerVerifiedUnit = liability
		s.Considered[i].MedianMsPerUnit = liabilities[candidate.CellID].MedianMsPerUnit
		if hardwareIdentity == "" {
			hardwareIdentity = liabilities[candidate.CellID].HardwareIdentity
		}
	}
	decision := decideMeasuredSupplierLiabilityShadow(liabilities, cells, s.RoutedCellID)
	if decision.EconomicsRefusal != "" {
		s.SupplierLiabilityHWClass = hw
		s.SupplierLiabilityHardwareIdentity = hardwareIdentity
		s.EconomicsRefusal = decision.EconomicsRefusal
		return s
	}
	if decision.Basis == "" || decision.Winner == "" {
		return s
	}
	s.ShadowCellID = decision.Winner
	s.SelectionBasis = decision.Basis
	s.SupplierLiabilityHWClass = hw
	s.SupplierLiabilityHardwareIdentity = hardwareIdentity
	return s
}

// measuredShadowDecision is the pure outcome of ranking measured candidates.
type measuredShadowDecision struct {
	Winner           string
	Basis            string
	EconomicsRefusal string
}

// decideMeasuredSupplierLiabilityShadow picks a winner only when the measured
// proxies establish equal supplier liability and throughput can decide. Pure:
// no DB, no admission side effects. Used by the live shadow path and governed
// comparison receipt so both surfaces cannot disagree on the refusal or basis.
func decideMeasuredSupplierLiabilityShadow(
	liabilities map[string]MeasuredSupplierLiabilityProxy, cells []string, routed string,
) measuredShadowDecision {
	ranked := rankCellsByMeasuredSupplierLiability(liabilities, cells)
	if len(ranked) < 2 {
		return measuredShadowDecision{}
	}
	bestLiability, okBest := measuredSupplierLiability(liabilities, ranked[0])
	secondLiability, okSecond := measuredSupplierLiability(liabilities, ranked[1])
	if !okBest || !okSecond {
		return measuredShadowDecision{}
	}
	if !supplierLiabilitiesTieUSD(bestLiability, secondLiability) {
		unknown := unresolvedPlatformCostComponents(liabilities[ranked[0]], liabilities[ranked[1]])
		return measuredShadowDecision{EconomicsRefusal: fmt.Sprintf(
			"cost-based shadow selection refused: measured supplier-liability proxies differ, but platform-cost components remain unknown (%v)",
			unknown)}
	}
	// Equal supplier liability. Throughput may rank only the cohort whose
	// liabilities tie with the best candidate. Passing all ranked candidates
	// through here would let a third, materially higher-liability cell win merely
	// because it was fastest even though only the two cheapest cells established
	// the equal-liability arm.
	liabilityTied := make([]string, 0, len(ranked))
	for _, cell := range ranked {
		liability, ok := measuredSupplierLiability(liabilities, cell)
		if ok && supplierLiabilitiesTieUSD(bestLiability, liability) {
			liabilityTied = append(liabilityTied, cell)
		}
	}
	byLatency := rankCellsByMeasuredLatency(liabilities, liabilityTied)
	if len(byLatency) < 2 {
		// Only one cell has a positive latency sample among the cost-tied set.
		if len(byLatency) == 1 {
			return measuredShadowDecision{
				Winner: byLatency[0], Basis: selectionBasisThroughputEqualLiability,
			}
		}
		return measuredShadowDecision{Winner: routed, Basis: selectionBasisTieNoDecision}
	}
	fast := liabilities[byLatency[0]].MedianMsPerUnit
	slow := liabilities[byLatency[1]].MedianMsPerUnit
	if latenciesTie(fast, slow) {
		// Every ranking term tied. Prefer the routed cell so sort order does not
		// manufacture a divergence, and name the refusal.
		winner := routed
		if winner == "" {
			winner = byLatency[0]
		}
		// The routed cell may be present with Measured=true yet still be excluded
		// by strict retry/verification/terminal evidence. Retain it only when it
		// belongs to this exact clean liability-tied latency cohort; otherwise a
		// TIE_NO_DECISION receipt would lend two clean arms' evidence to an
		// ineligible third arm.
		if !containsString(byLatency, winner) {
			winner = byLatency[0]
		}
		return measuredShadowDecision{Winner: winner, Basis: selectionBasisTieNoDecision}
	}
	return measuredShadowDecision{
		Winner: byLatency[0], Basis: selectionBasisThroughputEqualLiability,
	}
}

// supplierLiabilitiesTieUSD reports whether two supplier-liability proxies are
// the same number for the equal-liability throughput arm.
func supplierLiabilitiesTieUSD(a, b float64) bool {
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
func rankCellsByMeasuredLatency(liabilities map[string]MeasuredSupplierLiabilityProxy, cells []string) []string {
	type scored struct {
		cell string
		ms   float64
	}
	var ranked []scored
	for _, cell := range cells {
		c, ok := liabilities[cell]
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
		   considered_cells, excluded_cells, selection_policy, selection_basis_v3,
		   supplier_liability_hw_class, supplier_liability_hardware_identity,
		   economics_refusal, execution_mode,
		   execution_mode_reason, topology_plan)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20::jsonb)
		ON CONFLICT (job_id) DO NOTHING`,
		jobID, sel.RuntimeMatrixSHA, sel.PolicyRevision, sel.JobType, sel.ModelRef,
		sel.ModelKind, sel.WorkloadClass, sel.LatencyClass, sel.RoutedCellID,
		sel.ShadowCellID, string(considered), string(excluded), sel.SelectionPolicy,
		sel.SelectionBasis, sel.SupplierLiabilityHWClass,
		sel.SupplierLiabilityHardwareIdentity, sel.EconomicsRefusal,
		sel.ExecutionMode, sel.ExecutionModeReason, string(topology))
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
