package main

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"
)

// Measured per-cell execution cost, and the regret of the cell admission chose.
//
// runtime_shadow_selection.go recorded what a selector WOULD have chosen using
// the lifecycle ladder, and said plainly why it could go no further: "no per-cell
// source for latency, cost, memory, quality or failure exists in this tree
// today, and a scorer that invents them produces a number whose only property is
// that it looks like evidence."
//
// That source does exist — it was never read. Every committed task already
// carries the cell it ran on, the units it produced, the duration the worker
// reported, the supplier liability the plan froze, its retry count and its
// verification outcome. So the measurement is a QUERY, not a new writer and not
// a new table: adding either would have created a second, thinner copy of facts
// the money path already persists, and the two would have drifted.
//
// What this deliberately still does NOT model: storage, egress, provider energy,
// depreciation, model-load amortisation and refund risk. They are named
// `unknown` on the struct rather than defaulted to zero, because a zero reads as
// a measurement and is not one. The cost here is the cost of physically getting
// a verified unit out of a cell, which is the term a cell-versus-cell comparison
// actually turns on.

// minCellCostSamples is the fewest completed primary tasks a cell needs, in the
// EXACT scope, before its cost is treated as measured.
//
// Twenty, not one. A single task's duration is dominated by whether the model
// was already resident; the median of twenty is not. Below this the cell reports
// `Measured: false` and the selector falls back to the lifecycle ladder rather
// than ranking on a number drawn from three samples.
const minCellCostSamples = 20

// cellCostScope is the exact scope a measurement belongs to. Cost is not
// comparable across any of these, so none of them may be aggregated away: an
// embed cell's cost per unit says nothing about its generation cost, and a cost
// measured on apple_silicon_ultra says nothing about the same cell on a laptop.
type cellCostScope struct {
	JobType  string
	ModelRef string
	HWClass  string
}

func (s cellCostScope) String() string {
	return fmt.Sprintf("%s/%s/%s", s.JobType, s.ModelRef, s.HWClass)
}

// MeasuredCellCost is what one cell actually cost, per unit of delivered work.
type MeasuredCellCost struct {
	CellID    string `json:"cell_id"`
	RuntimeID string `json:"runtime_id"`
	Engine    string `json:"engine"`
	JobType   string `json:"job_type"`
	ModelRef  string `json:"model_ref"`
	HWClass   string `json:"hw_class"`

	Samples int   `json:"samples"`
	Units   int64 `json:"units"`

	// MedianMsPerUnit is the median over TASKS of (duration / units), not the
	// total duration over the total units. The second form lets one large task
	// dominate, which is how a cell that is fast in bulk and slow per request
	// gets recorded as uniformly fast.
	MedianMsPerUnit     float64 `json:"median_ms_per_unit"`
	SupplierUSDPerUnit  float64 `json:"supplier_usd_per_unit"`
	RetryRate           float64 `json:"retry_rate"`
	VerificationSamples int     `json:"verification_samples"`
	VerificationFails   int     `json:"verification_fails"`

	// Terminal execution outcomes for this cell, over a WIDER set than Samples:
	// every primary task that reached a terminal state on it, including the ones
	// that failed outright and therefore delivered no units at all.
	//
	// Without this the measurement had a hole big enough to promote a crashing
	// cell. Samples counts completed tasks only, so a cell that failed half the
	// work it claimed looked exactly as clean as one that failed none — the
	// failures simply were not in the sample. The verification term catches a
	// REJECTED result; nothing caught a task that never produced one.
	TerminalAttempts int `json:"terminal_attempts"`
	TerminalFails    int `json:"terminal_fails"`

	// Measured reports whether this row cleared minCellCostSamples. A caller
	// that ranks on an unmeasured row is ranking on noise, so the flag travels
	// with the number instead of being recomputed by every reader.
	Measured bool `json:"measured"`

	// Unknown names every cost component this measurement does not contain. It
	// is part of the value, not documentation: a reader comparing two cells has
	// to know what the comparison leaves out.
	Unknown []string `json:"unknown_components"`
}

// unknownCostComponents is the same list for every cell, stated once.
func unknownCostComponents() []string {
	return []string{
		"storage", "egress", "provider_energy", "depreciation",
		"model_load_amortisation", "refund_risk",
	}
}

// ExpectedVerifiedOutcomeUSDPerUnit is the cost of a unit that PASSES.
//
//	supplier cost per unit
//	  × (1 + retry rate)           -- the attempts actually observed, not one
//	  ÷ (1 - verification failure)  -- a rejected result buys nothing and re-runs
//	  ÷ (1 - terminal failure)      -- a crashed task buys nothing either
//
// The two divisions are what make this a verified-OUTCOME cost rather than an
// execution cost. A cell 30% cheaper per attempt that fails verification a third
// of the time is not cheaper; neither is one that fails outright a third of the
// time, and only the second denominator catches that — the sample of completed
// tasks cannot, because a task that crashed is not in it.
//
// ok is false when the cell has no verified-outcome cost at all (everything
// rejected or everything failed), which is a refusal to divide by zero and call
// the answer infinite.
func (c MeasuredCellCost) ExpectedVerifiedOutcomeUSDPerUnit() (float64, bool) {
	if c.SupplierUSDPerUnit <= 0 {
		return 0, false
	}
	pass := 1.0
	if c.VerificationSamples > 0 {
		pass = 1 - float64(c.VerificationFails)/float64(c.VerificationSamples)
	}
	delivered := 1.0
	if c.TerminalAttempts > 0 {
		delivered = 1 - float64(c.TerminalFails)/float64(c.TerminalAttempts)
	}
	if pass <= 0 || delivered <= 0 {
		return 0, false
	}
	cost := c.SupplierUSDPerUnit * (1 + c.RetryRate) / pass / delivered
	if math.IsNaN(cost) || math.IsInf(cost, 0) {
		return 0, false
	}
	return cost, true
}

// MeasuredCellCostsByHardware reads the cost of every cell that has completed
// primary work for one workload and model, grouped by the hardware class it ran
// on and then by cell.
//
// Hardware is the outer key and never aggregated away. Comparing a cell measured
// on an M3 Ultra against a cell measured on a laptop produces a number whose
// magnitude is the hardware, not the runtime — and the whole point of a selector
// is to attribute the difference to the runtime.
//
// Only PRIMARY tasks count. A honeypot, a redundancy check and a hedge are
// verification and scheduling overhead: they are real cost, but they are not the
// cost of delivering a unit, and folding them in would make a cell look more
// expensive exactly when Merc chose to check it more carefully.
func (s *Store) MeasuredCellCostsByHardware(
	ctx context.Context, jobType, modelRef string,
) (map[string]map[string]MeasuredCellCost, error) {
	rows, err := s.pool.Query(ctx, `
		WITH primary_tasks AS (
		  SELECT t.runtime_cell_id AS cell_id,
		         COALESCE(t.execution_hw_class,'')  AS hw_class,
		         COALESCE(t.runtime_id,'')          AS runtime_id,
		         COALESCE(t.execution_engine,'')    AS engine,
		         COALESCE(t.expected_output_records,0) AS units,
		         t.reported_duration_ms             AS duration_ms,
		         COALESCE(t.economic_supplier_payout_usd,0)::float8 AS supplier_usd,
		         COALESCE(t.retry_count,0)          AS retries,
		         t.verification_outcome             AS outcome
		    FROM tasks t
		    JOIN jobs j ON j.id = t.job_id
		   WHERE t.status = 'complete'
		     AND t.runtime_cell_id IS NOT NULL
		     AND NOT COALESCE(t.is_honeypot,false)
		     AND NOT COALESCE(t.is_redundancy,false)
		     AND t.hedged_from IS NULL
		     AND t.reported_duration_ms IS NOT NULL
		     AND COALESCE(t.expected_output_records,0) > 0
		     AND j.job_type = $1
		     AND COALESCE(j.model_ref,'') = $2
		), terminal_tasks AS (
		  -- Wider than primary_tasks on purpose: every primary task that reached a
		  -- terminal state ON this cell, including the ones that failed and
		  -- therefore have no duration, no units and no verification outcome. A
		  -- task that failed before it was ever claimed has no execution hardware
		  -- and is not this cell's failure, so it is excluded.
		  SELECT t.runtime_cell_id AS cell_id,
		         COALESCE(t.execution_hw_class,'') AS hw_class,
		         t.status
		    FROM tasks t
		    JOIN jobs j ON j.id = t.job_id
		   WHERE t.status IN ('complete','failed')
		     AND t.runtime_cell_id IS NOT NULL
		     AND COALESCE(t.execution_hw_class,'') <> ''
		     AND NOT COALESCE(t.is_honeypot,false)
		     AND NOT COALESCE(t.is_redundancy,false)
		     AND t.hedged_from IS NULL
		     AND j.job_type = $1
		     AND COALESCE(j.model_ref,'') = $2
		), terminal_rollup AS (
		  SELECT hw_class, cell_id,
		         COUNT(*)::int AS attempts,
		         COUNT(*) FILTER (WHERE status = 'failed')::int AS fails
		    FROM terminal_tasks GROUP BY hw_class, cell_id
		)
		SELECT p.hw_class, p.cell_id,
		       MIN(p.runtime_id), MIN(p.engine),
		       COUNT(*)::int,
		       SUM(p.units)::bigint,
		       percentile_cont(0.5) WITHIN GROUP (
		         ORDER BY p.duration_ms::float8 / p.units::float8),
		       SUM(p.supplier_usd),
		       SUM(p.retries)::float8 / COUNT(*)::float8,
		       COUNT(*) FILTER (WHERE p.outcome IS NOT NULL)::int,
		       COUNT(*) FILTER (WHERE p.outcome IS NOT NULL AND p.outcome <> 'pass')::int,
		       COALESCE(MIN(r.attempts), 0), COALESCE(MIN(r.fails), 0)
		  FROM primary_tasks p
		  LEFT JOIN terminal_rollup r
		         ON r.hw_class = p.hw_class AND r.cell_id = p.cell_id
		 GROUP BY p.hw_class, p.cell_id
		 ORDER BY p.hw_class, p.cell_id`, jobType, modelRef)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string]map[string]MeasuredCellCost{}
	for rows.Next() {
		var c MeasuredCellCost
		var supplierTotal float64
		if err := rows.Scan(&c.HWClass, &c.CellID, &c.RuntimeID, &c.Engine, &c.Samples,
			&c.Units, &c.MedianMsPerUnit, &supplierTotal, &c.RetryRate,
			&c.VerificationSamples, &c.VerificationFails,
			&c.TerminalAttempts, &c.TerminalFails); err != nil {
			return nil, err
		}
		c.JobType, c.ModelRef = jobType, modelRef
		if c.Units > 0 {
			c.SupplierUSDPerUnit = supplierTotal / float64(c.Units)
		}
		c.Measured = c.Samples >= minCellCostSamples
		c.Unknown = unknownCostComponents()
		if out[c.HWClass] == nil {
			out[c.HWClass] = map[string]MeasuredCellCost{}
		}
		out[c.HWClass][c.CellID] = c
	}
	return out, rows.Err()
}

// MeasuredCellCosts is one hardware class of the above.
func (s *Store) MeasuredCellCosts(
	ctx context.Context, scope cellCostScope,
) (map[string]MeasuredCellCost, error) {
	byHW, err := s.MeasuredCellCostsByHardware(ctx, scope.JobType, scope.ModelRef)
	if err != nil {
		return nil, err
	}
	if byHW[scope.HWClass] == nil {
		return map[string]MeasuredCellCost{}, nil
	}
	return byHW[scope.HWClass], nil
}

// comparableHardwareFor picks the hardware class a candidate set can honestly be
// compared on: the one where the most candidates are measured, breaking ties on
// total samples and then on name so the choice is deterministic.
//
// It returns "" when no hardware class has two measured candidates, which is a
// refusal to compare rather than a fallback to comparing across hardware.
func comparableHardwareFor(
	byHW map[string]map[string]MeasuredCellCost, candidates []string,
) string {
	bestHW, bestCovered, bestSamples := "", 0, 0
	for _, hw := range sortedKeys(byHW) {
		covered, samples := 0, 0
		for _, cell := range candidates {
			c, ok := byHW[hw][cell]
			if !ok || !c.Measured {
				continue
			}
			covered++
			samples += c.Samples
		}
		if covered < 2 {
			continue
		}
		if covered > bestCovered || (covered == bestCovered && samples > bestSamples) {
			bestHW, bestCovered, bestSamples = hw, covered, samples
		}
	}
	return bestHW
}

// SelectorRegret is what one scope's routing decisions cost against the cheapest
// measured eligible cell.
//
// Decisions whose candidate set is not fully measured are COUNTED, not scored.
// Dropping them would let a scope with two measured decisions and four hundred
// unmeasured ones report a confident zero, which is the failure mode this whole
// file exists to avoid.
type SelectorRegret struct {
	JobType  string `json:"job_type"`
	ModelRef string `json:"model_ref"`
	HWClass  string `json:"hw_class"`

	Decisions           int `json:"decisions"`
	ScoredDecisions     int `json:"scored_decisions"`
	UnmeasuredDecisions int `json:"unmeasured_decisions"`
	DivergedDecisions   int `json:"diverged_decisions"`

	// Regret is per unit, in settlement currency, of the routed cell against the
	// cheapest measured eligible cell in the same decision. Zero means the
	// routed cell WAS the cheapest, which is the result to hope for and not the
	// result to assume.
	TotalRegretPerUnit float64 `json:"total_regret_per_unit"`
	MaxRegretPerUnit   float64 `json:"max_regret_per_unit"`
	MeanRegretPerUnit  float64 `json:"mean_regret_per_unit"`

	// CheapestCell is the cell that would have won every scored decision, or ""
	// when the scored decisions do not agree on one.
	CheapestCell string `json:"cheapest_measured_cell"`
}

// shadowDecisionRow is the part of a recorded decision regret needs.
type shadowDecisionRow struct {
	JobType    string
	ModelRef   string
	RoutedCell string
	ShadowCell string
	Considered []string
}

// SelectorRegretForScope pairs recorded shadow decisions with measured cell cost.
//
// hwClass is a parameter rather than read off the decision because a decision is
// recorded at admission, before any worker has been chosen: the hardware a job
// ran on is a property of the execution, and asking a decision what hardware it
// used would be asking it something it cannot know.
func (s *Store) SelectorRegretForScope(
	ctx context.Context, scope cellCostScope,
) (SelectorRegret, map[string]MeasuredCellCost, error) {
	costs, err := s.MeasuredCellCosts(ctx, scope)
	if err != nil {
		return SelectorRegret{}, nil, err
	}
	rows, err := s.pool.Query(ctx, `
		SELECT job_type, model_ref, routed_cell_id, shadow_cell_id,
		       COALESCE((
		         SELECT array_agg(c->>'cell_id' ORDER BY c->>'cell_id')
		           FROM jsonb_array_elements(considered_cells) c), '{}')
		  FROM runtime_shadow_selections
		 WHERE job_type = $1 AND model_ref = $2
		 ORDER BY decided_at`, scope.JobType, scope.ModelRef)
	if err != nil {
		return SelectorRegret{}, nil, err
	}
	defer rows.Close()

	out := SelectorRegret{JobType: scope.JobType, ModelRef: scope.ModelRef, HWClass: scope.HWClass}
	winners := map[string]int{}
	for rows.Next() {
		var d shadowDecisionRow
		if err := rows.Scan(&d.JobType, &d.ModelRef, &d.RoutedCell, &d.ShadowCell, &d.Considered); err != nil {
			return SelectorRegret{}, nil, err
		}
		out.Decisions++
		if d.RoutedCell != d.ShadowCell {
			out.DivergedDecisions++
		}
		regret, cheapest, ok := scoreDecisionRegret(d, costs)
		if !ok {
			out.UnmeasuredDecisions++
			continue
		}
		out.ScoredDecisions++
		out.TotalRegretPerUnit += regret
		if regret > out.MaxRegretPerUnit {
			out.MaxRegretPerUnit = regret
		}
		winners[cheapest]++
	}
	if err := rows.Err(); err != nil {
		return SelectorRegret{}, nil, err
	}
	if out.ScoredDecisions > 0 {
		out.MeanRegretPerUnit = out.TotalRegretPerUnit / float64(out.ScoredDecisions)
	}
	if len(winners) == 1 {
		for cell := range winners {
			out.CheapestCell = cell
		}
	}
	return out, costs, nil
}

// handleAdminSelectorRegret exposes the same measured-outcome regret used by
// the promotion gate. It is read-only and admin-scoped: a public caller must
// not be able to turn a partially measured scope into a routing claim, and an
// operator needs the explicit unmeasured count to know when the report is not
// promotion evidence.
func (s *Server) handleAdminSelectorRegret(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	scope := cellCostScope{
		JobType:  strings.TrimSpace(query.Get("job_type")),
		ModelRef: strings.TrimSpace(query.Get("model_ref")),
		HWClass:  strings.TrimSpace(query.Get("hw_class")),
	}
	if scope.JobType == "" || scope.ModelRef == "" || scope.HWClass == "" {
		writeErr(w, http.StatusBadRequest, "job_type, model_ref, and hw_class are required")
		return
	}
	regret, costs, err := s.store.SelectorRegretForScope(r.Context(), scope)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "selector regret unavailable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"regret":         regret,
		"measured_costs": costs,
	})
}

// handleAdminSelectorPromotion evaluates, but never applies, the narrow cell
// promotion gate. Requiring every scope dimension in the query keeps an
// operator from turning a partial regret report into a fleet-wide promotion;
// the response is a refusal-preserving evidence receipt whose digest can be
// carried into the separate activation-policy write after review.
func (s *Server) handleAdminSelectorPromotion(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	value := func(name string) string { return strings.TrimSpace(query.Get(name)) }
	scope := CellPromotionScope{
		JobType:       value("job_type"),
		ModelRef:      value("model_ref"),
		ModelRevision: value("model_revision"),
		QualityTier:   value("quality_tier"),
		Verification:  value("verification_contract"),
		HWClass:       value("hw_class"),
		LatencyClass:  value("latency_class"),
		RuntimeID:     value("runtime_id"),
		CellID:        value("cell_id"),
	}
	incumbent := value("incumbent_cell")
	missing := make([]string, 0, 10)
	for name, field := range map[string]string{
		"job_type":              scope.JobType,
		"model_ref":             scope.ModelRef,
		"model_revision":        scope.ModelRevision,
		"quality_tier":          scope.QualityTier,
		"verification_contract": scope.Verification,
		"hw_class":              scope.HWClass,
		"latency_class":         scope.LatencyClass,
		"runtime_id":            scope.RuntimeID,
		"cell_id":               scope.CellID,
		"incumbent_cell":        incumbent,
	} {
		if field == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		writeErr(w, http.StatusBadRequest, "missing selector promotion scope: "+strings.Join(missing, ", "))
		return
	}
	evidence, err := s.store.EvaluateCellPromotion(r.Context(), scope, incumbent, time.Now().UTC())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "selector promotion evaluation unavailable")
		return
	}
	digest, err := evidence.Digest()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "selector promotion receipt unavailable")
		return
	}
	receiptRef, err := evidence.ReceiptRef()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "selector promotion receipt unavailable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"passed":                evidence.Passed(),
		"evidence":              evidence,
		"evidence_sha256":       digest,
		"promotion_receipt_ref": receiptRef,
		"activation_applied":    false,
	})
}

// scoreDecisionRegret returns the routed cell's regret against the cheapest
// measured cell it was considered against.
//
// ok is false unless the routed cell AND at least one other considered cell are
// measured. A decision scored against a candidate set where only the winner has
// data would always report zero regret, which is not evidence that the winner
// was right — it is evidence that nothing else was tried.
func scoreDecisionRegret(
	d shadowDecisionRow, costs map[string]MeasuredCellCost,
) (regret float64, cheapest string, ok bool) {
	routed, hasRouted := measuredCost(costs, d.RoutedCell)
	if !hasRouted {
		return 0, "", false
	}
	best, bestCell, others := routed, d.RoutedCell, 0
	for _, cell := range d.Considered {
		if cell == d.RoutedCell {
			continue
		}
		cost, has := measuredCost(costs, cell)
		if !has {
			continue
		}
		others++
		if cost < best {
			best, bestCell = cost, cell
		}
	}
	if others == 0 {
		return 0, "", false
	}
	return routed - best, bestCell, true
}

// measuredCost is the verified-outcome cost of a cell, or false when the cell is
// absent, under-sampled, or has no verified-outcome cost at all.
func measuredCost(costs map[string]MeasuredCellCost, cell string) (float64, bool) {
	c, ok := costs[cell]
	if !ok || !c.Measured {
		return 0, false
	}
	return c.ExpectedVerifiedOutcomeUSDPerUnit()
}

// rankCellsByMeasuredCost orders cell ids cheapest-first, dropping unmeasured
// cells. Ties break on cell id so the order is stable across runs; a caller that
// wants the incumbent to win a tie has to say so itself.
func rankCellsByMeasuredCost(costs map[string]MeasuredCellCost, cells []string) []string {
	type scored struct {
		cell string
		cost float64
	}
	var ranked []scored
	for _, cell := range cells {
		cost, ok := measuredCost(costs, cell)
		if !ok {
			continue
		}
		ranked = append(ranked, scored{cell, cost})
	}
	sort.Slice(ranked, func(i, j int) bool {
		if ranked[i].cost != ranked[j].cost {
			return ranked[i].cost < ranked[j].cost
		}
		return ranked[i].cell < ranked[j].cell
	})
	out := make([]string, 0, len(ranked))
	for _, r := range ranked {
		out = append(out, r.cell)
	}
	return out
}
