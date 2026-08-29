package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Measured per-cell supplier-liability proxy, and the liability regret of the
// cell admission chose.
//
// runtime_shadow_selection.go recorded what a selector WOULD have chosen using
// the lifecycle ladder, and said plainly why it could go no further: "no per-cell
// source for latency, cost, memory, quality or failure exists in this tree
// today, and a scorer that invents them produces a number whose only property is
// that it looks like evidence."
//
// That source does exist — it was never read. Every committed task already
// carries the cell it ran on, the units it produced, the duration the worker
// reported, its retry count and its verification outcome. The immutable ledger
// carries the exact supplier liability settlement. So the measurement is a
// QUERY, not a new writer and not a new table: adding either would have created
// a second, thinner copy of facts the execution and money paths already persist,
// and the copies would have drifted.
//
// What this deliberately still does NOT model: storage, egress, provider energy,
// depreciation, model-load amortisation and refund risk. They are named
// `unknown_platform_cost_components` on the struct rather than defaulted to
// zero, because a zero reads as a measurement and is not one. The money number
// here is only the eventual accepted supplier payout. Retry, verification and
// terminal-failure observations travel beside it as reliability evidence; they
// never manufacture supplier liability for work that settlement does not pay.
// It is not a complete cost of producing or delivering a verified outcome.

// minSupplierLiabilitySamples is the fewest completed primary tasks a cell
// needs, in the EXACT scope, before its supplier-liability proxy is treated as
// measured.
//
// Twenty, not one. A single task's duration is dominated by whether the model
// was already resident; the median of twenty is not. Below this the cell reports
// `Measured: false` and the selector falls back to the lifecycle ladder rather
// than ranking on a number drawn from three samples.
const minSupplierLiabilitySamples = 20

// supplierLiabilityScope is the exact scope a measurement belongs to. Supplier
// liability is not comparable across any of these, so none of them may be
// aggregated away: an embed cell's payout per output says nothing about its
// generation payout, and a latency measured on apple_silicon_ultra says nothing
// about the same cell on a laptop. The read also requires one exact frozen
// input/task geometry inside the scope. That last dimension is derived from the
// immutable ComputePlan, PricingDecision billable units, and task output/depth
// authority rather than accepted from an operator.
type supplierLiabilityScope struct {
	JobType          string
	ModelRef         string
	HWClass          string
	HardwareIdentity string

	// The remaining fields are the immutable contract/epoch dimensions that
	// make two task observations comparable. Leaving one empty is not a broad
	// query: validate refuses the scope. A liability sample pooled across price
	// tiers, currencies, schedules, runtime matrices, model revisions, quality
	// contracts, verification contracts, latency classes, policy revisions, or
	// an unbounded time range is not evidence about any one of them.
	Tier                    string
	Currency                string
	CatalogueScheduleSHA256 string
	RuntimeMatrixSHA256     string
	ModelRevision           string
	QualityTier             string
	Verification            string
	LatencyClass            string
	SelectionPolicy         string
	PolicyRevision          int64
	ObservedAfter           time.Time
	ObservedBefore          time.Time

	// A promotion scope names the exact incumbent/challenger pair. Empty on the
	// read-only fleet report; both-or-neither is required.
	IncumbentCell  string
	ChallengerCell string
}

func (s supplierLiabilityScope) String() string {
	return fmt.Sprintf("%s/%s/%s/%s tier=%s currency=%s schedule=%s matrix=%s model_revision=%s quality=%s verification=%s latency=%s policy=%s@%d window=[%s,%s] pair=%s->%s",
		s.JobType, s.ModelRef, s.HWClass, s.HardwareIdentity, s.Tier, s.Currency,
		s.CatalogueScheduleSHA256, s.RuntimeMatrixSHA256, s.ModelRevision,
		s.QualityTier, s.Verification, s.LatencyClass, s.SelectionPolicy,
		s.PolicyRevision, s.ObservedAfter.UTC().Format(time.RFC3339Nano),
		s.ObservedBefore.UTC().Format(time.RFC3339Nano), s.IncumbentCell,
		s.ChallengerCell)
}

const supplierLiabilityObservationWindow = 30 * 24 * time.Hour

func (s supplierLiabilityScope) validate() error {
	missing := make([]string, 0, 12)
	for name, value := range map[string]string{
		"job_type": s.JobType, "model_ref": s.ModelRef, "hw_class": s.HWClass,
		"hardware_identity": s.HardwareIdentity,
		"tier":              s.Tier, "currency": s.Currency,
		"catalogue_schedule_sha256": s.CatalogueScheduleSHA256,
		"runtime_matrix_sha256":     s.RuntimeMatrixSHA256,
		"model_revision":            s.ModelRevision, "quality_tier": s.QualityTier,
		"verification_contract": s.Verification, "latency_class": s.LatencyClass,
		"selection_policy": s.SelectionPolicy,
	} {
		if strings.TrimSpace(value) == "" {
			missing = append(missing, name)
		}
	}
	if s.PolicyRevision <= 0 {
		missing = append(missing, "policy_revision")
	}
	if s.ObservedAfter.IsZero() || s.ObservedBefore.IsZero() ||
		!s.ObservedAfter.Before(s.ObservedBefore) {
		missing = append(missing, "bounded_observation_window")
	} else if s.ObservedBefore.Sub(s.ObservedAfter) > supplierLiabilityObservationWindow {
		return fmt.Errorf("supplier-liability observation window %s exceeds governed maximum %s",
			s.ObservedBefore.Sub(s.ObservedAfter), supplierLiabilityObservationWindow)
	}
	if (s.IncumbentCell == "") != (s.ChallengerCell == "") {
		missing = append(missing, "complete_incumbent_challenger_pair")
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return fmt.Errorf("exact supplier-liability scope unavailable: %s",
			strings.Join(missing, ", "))
	}
	if !validCanonicalHardwareIdentity(s.HardwareIdentity) {
		return fmt.Errorf("exact supplier-liability scope has invalid hardware_identity %q",
			s.HardwareIdentity)
	}
	if !validSHA256(s.CatalogueScheduleSHA256) || !validSHA256(s.RuntimeMatrixSHA256) {
		return errors.New("exact supplier-liability scope has an invalid schedule or runtime-matrix digest")
	}
	return nil
}

// supplierLiabilityScopeForShadow freezes every persisted authority dimension
// needed to compare prior observations for one new shadow decision. It refuses
// when the accepted workload/pricing decision and selector row do not describe
// the same contract; callers must not repair such disagreement with defaults.
func supplierLiabilityScopeForShadow(
	shadow ShadowSelection,
	workload WorkloadDecision,
	pricing PricingDecision,
	hwClass, hardwareIdentity string,
	observedBefore time.Time,
) (supplierLiabilityScope, error) {
	if shadow.JobType != workload.RuntimeJobType ||
		shadow.ModelRef != workload.Binding.Model.Ref ||
		shadow.LatencyClass != workload.LatencyClass {
		return supplierLiabilityScope{}, errors.New(
			"exact supplier-liability scope unavailable: shadow selection disagrees with frozen workload authority")
	}
	if pricing.Tier != workload.Binding.Tier {
		return supplierLiabilityScope{}, errors.New(
			"exact supplier-liability scope unavailable: frozen pricing tier disagrees with workload tier")
	}
	var routed *shadowCandidate
	for i := range shadow.Considered {
		candidate := &shadow.Considered[i]
		if candidate.CellID != shadow.RoutedCellID {
			continue
		}
		if routed != nil {
			return supplierLiabilityScope{}, fmt.Errorf(
				"exact supplier-liability scope unavailable: routed cell %s has duplicate contract rows",
				shadow.RoutedCellID)
		}
		routed = candidate
	}
	if routed == nil {
		return supplierLiabilityScope{}, fmt.Errorf(
			"exact supplier-liability scope unavailable: routed cell %s has no persisted contract row",
			shadow.RoutedCellID)
	}
	observedBefore = observedBefore.UTC()
	scope := supplierLiabilityScope{
		JobType: shadow.JobType, ModelRef: shadow.ModelRef,
		HWClass:          strings.TrimSpace(hwClass),
		HardwareIdentity: strings.TrimSpace(hardwareIdentity), Tier: pricing.Tier,
		Currency:                pricing.Currency,
		CatalogueScheduleSHA256: pricing.Catalogue.ScheduleSHA256,
		RuntimeMatrixSHA256:     shadow.RuntimeMatrixSHA,
		ModelRevision:           workload.ModelRevision,
		QualityTier:             routed.QualityTier, Verification: routed.Verification,
		LatencyClass: shadow.LatencyClass, SelectionPolicy: shadow.SelectionPolicy,
		PolicyRevision: shadow.PolicyRevision,
		ObservedAfter:  observedBefore.Add(-supplierLiabilityObservationWindow),
		ObservedBefore: observedBefore,
	}
	if err := scope.validate(); err != nil {
		return supplierLiabilityScope{}, err
	}
	return scope, nil
}

// resolveSupplierLiabilityEconomicEpoch derives the one currency/catalogue
// schedule represented by immutable job decisions in an otherwise exact
// observation scope. A promotion/report does not name one accepted job, so it
// may proceed only when the bounded cohort has exactly one such epoch. Zero or
// multiple epochs are a refusal; choosing the latest would silently pool or
// discard authority.
func (s *Store) resolveSupplierLiabilityEconomicEpoch(
	ctx context.Context, scope supplierLiabilityScope,
) (currency, scheduleSHA256 string, err error) {
	rows, err := s.pool.Query(ctx, `
		SELECT DISTINCT j.currency,
		       j.pricing_decision #>> '{catalogue,schedule_sha256}'
		  FROM jobs j
		  JOIN runtime_shadow_selections rs ON rs.job_id = j.id
		 WHERE j.job_type = $1
		   AND COALESCE(j.model_ref,'') = $2
		   AND j.tier = $3
		   AND j.workload_decision ->> 'model_revision' = $4
		   AND j.workload_decision ->> 'latency_class' = $5
		   AND rs.runtime_matrix_sha256 = $6
		   AND rs.policy_revision = $7
		   AND rs.selection_policy = $8
		   AND rs.latency_class = $5
		   AND rs.decided_at >= $9 AND rs.decided_at <= $10
		   AND EXISTS (
		     SELECT 1 FROM jsonb_array_elements(rs.considered_cells) candidate
		      WHERE candidate->>'quality_tier' = $11
		        AND candidate->>'verification' = $12)
		 ORDER BY 1,2`,
		scope.JobType, scope.ModelRef, scope.Tier, scope.ModelRevision,
		scope.LatencyClass, scope.RuntimeMatrixSHA256, scope.PolicyRevision,
		scope.SelectionPolicy, scope.ObservedAfter, scope.ObservedBefore,
		scope.QualityTier, scope.Verification)
	if err != nil {
		return "", "", err
	}
	defer rows.Close()
	type epoch struct{ currency, schedule string }
	var epochs []epoch
	for rows.Next() {
		var e epoch
		if err := rows.Scan(&e.currency, &e.schedule); err != nil {
			return "", "", err
		}
		epochs = append(epochs, e)
	}
	if err := rows.Err(); err != nil {
		return "", "", err
	}
	switch len(epochs) {
	case 1:
		if strings.TrimSpace(epochs[0].currency) == "" || !validSHA256(epochs[0].schedule) {
			return "", "", errors.New(
				"exact supplier-liability economic epoch unavailable: persisted currency or catalogue schedule is incomplete")
		}
		return epochs[0].currency, epochs[0].schedule, nil
	case 0:
		return "", "", errors.New(
			"exact supplier-liability economic epoch unavailable: no immutable job/shadow authority exists in the bounded scope")
	default:
		return "", "", fmt.Errorf(
			"exact supplier-liability economic epoch unavailable: bounded scope contains %d currency/catalogue schedules",
			len(epochs))
	}
}

// MeasuredSupplierLiabilityProxy is the frozen supplier liability observed for
// one cell together with separate reliability evidence. SupplierUSDPerUnit is
// the eventual accepted supplier payout per delivered unit; a rejected or
// failed attempt receives no settlement and therefore cannot be multiplied
// into this money value. Reliability instead gates whether the observation may
// participate in selector or promotion decisions. This remains a proxy, not
// platform cost: it contains no storage, egress, energy, depreciation,
// model-load, utilization, or refund-risk measurement.
type MeasuredSupplierLiabilityProxy struct {
	CellID    string `json:"cell_id"`
	RuntimeID string `json:"runtime_id"`
	Engine    string `json:"engine"`
	JobType   string `json:"job_type"`
	ModelRef  string `json:"model_ref"`
	HWClass   string `json:"hw_class"`
	// HardwareIdentity is the exact device generation/model, not the broad
	// capacity class. Two Apple Ultra generations are different physical
	// authorities even when they share HWClass.
	HardwareIdentity string `json:"hardware_identity"`
	// Currency is the settlement-major-unit authority for every money field.
	// Historical `_usd` field names are retained on the wire for compatibility;
	// they do not override this explicit ISO code.
	Currency string `json:"currency"`

	Samples int   `json:"samples"`
	Units   int64 `json:"units"`

	// MedianMsPerUnit is the median over TASKS of (duration / delivered output),
	// not the
	// total duration over the total units. The second form lets one large task
	// dominate, which is how a cell that is fast in bulk and slow per request
	// gets recorded as uniformly fast. Output-normalised latency remains
	// traffic-mix dependent, so this value is rankable only when
	// InputGeometrySHA256 is present and identical across the cohort.
	MedianMsPerUnit float64 `json:"median_ms_per_unit"`
	// SupplierUSDPerUnit is accepted payout / delivered outputs. That is a
	// truthful outcome ratio, but it is not the catalogue's canonical unit price:
	// catalogue money is denominated by PricingDecision.BillableUnits, which can
	// be driven by raw input depth even when output count is unchanged. Payout per
	// billable settlement unit is useful for money reconciliation but, under one
	// schedule and supplier share, is fixed by contract and cannot distinguish
	// runtime cells. The output ratio is therefore retained only behind the exact
	// geometry gate recorded below.
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

	// Measured reports whether this row cleared the completed, verification, and
	// terminal evidence floors without a retry, rejected verification, or terminal
	// failure. Until a separate governed reliability threshold exists, any observed
	// retry is a fail-closed execution burden rather than a ranking input. A caller
	// that ranks on an unmeasured row is ranking on noise or known reliability
	// failure, so the flag travels with the number instead of being recomputed by
	// every reader. measuredSupplierLiability rechecks the invariants so a hand-built
	// or stale row cannot bypass them by setting this bit.
	Measured bool `json:"measured"`

	// SourceBinding is the binding_status of the artifact these numbers came
	// out of, carried on the row rather than looked up by each reader.
	//
	// A measurement is only as bound as the evidence it reads. A derived
	// receipt stamps BOUND from the identity of the harness that ran the
	// projection, which says who did the arithmetic and nothing at all about
	// who produced the inputs. That gap is how an UNBOUND cohort file --
	// missing source_commit, build_digest, model_artifact_digest and
	// raw_samples, so unable to name which binary produced its timings -- can
	// end up backing a BOUND-stamped verdict about which engine is faster.
	//
	// Empty means the caller did not say, which is treated as not bound.
	SourceBinding string `json:"source_binding,omitempty"`

	// Exact observation identity. Production selector/promotion reads populate
	// these from immutable task/job authority. A cell observed under more than
	// one runtime or execution build in the requested window is returned as
	// unmeasured with AuthorityRefusal set; silently pooling builds would let an
	// old binary lend samples to a new challenger.
	ExecutionBuildHash           string `json:"execution_build_hash,omitempty"`
	ExecutionBuildIdentityPolicy string `json:"execution_build_identity_policy,omitempty"`
	RuntimeMatrixSHA256          string `json:"runtime_matrix_sha256,omitempty"`
	// InputGeometrySHA256 is a digest of the one canonical geometry represented
	// by the cohort: frozen ComputePlan input records/bytes/depth/split/task
	// counts, PricingDecision billable units, and task expected-output/depth
	// authority. Empty means that geometry was incomplete or mixed and the row
	// is ineligible. This is a digest of existing authorities, not a new pricing
	// input.
	InputGeometrySHA256 string   `json:"input_geometry_sha256,omitempty"`
	AuthorityRefusals   []string `json:"authority_refusals,omitempty"`

	// UnknownPlatformCostComponents names every platform-cost component this
	// proxy does not contain. It is part of the value, not documentation: a
	// reader comparing two cells has to know what the comparison leaves out.
	UnknownPlatformCostComponents []string `json:"unknown_platform_cost_components"`
}

// unknownPlatformCostComponents is the same list for every cell, stated once.
func unknownPlatformCostComponents() []string {
	return []string{
		"storage", "egress", "provider_energy", "depreciation",
		"model_load_amortisation", "refund_risk",
	}
}

// unresolvedPlatformCostComponents returns the stable union carried by a set of
// supplier-liability proxies. An omitted list is not evidence that every cost
// was measured: this value has no fields capable of carrying those measurements,
// so omission falls back to the canonical unknown set.
func unresolvedPlatformCostComponents(proxies ...MeasuredSupplierLiabilityProxy) []string {
	set := map[string]struct{}{}
	for _, proxy := range proxies {
		components := proxy.UnknownPlatformCostComponents
		if len(components) == 0 {
			components = unknownPlatformCostComponents()
		}
		for _, component := range components {
			component = strings.TrimSpace(component)
			if component != "" {
				set[component] = struct{}{}
			}
		}
	}
	out := make([]string, 0, len(set))
	for component := range set {
		out = append(out, component)
	}
	sort.Strings(out)
	return out
}

// ExpectedSupplierLiabilityUSDPerVerifiedUnit returns the eventual accepted
// supplier payout per delivered unit. The historical method name is retained
// for receipt/schema compatibility, but this is no longer a reliability-
// adjusted expectation: settlement pays the accepted result once and pays
// neither a rejected verification nor a terminally failed attempt. Multiplying
// by retries or dividing by failure rates would charge money the supplier never
// receives.
//
// Reliability is deliberately not discarded. VerificationSamples,
// VerificationFails, TerminalAttempts, TerminalFails, and RetryRate remain on
// the proxy, and measuredSupplierLiability fail-closes selector/promotion use
// when retry burden is nonzero or verification/terminal evidence is
// insufficient or has any failure.
// ok is false only when the accepted supplier payout itself is unavailable or
// not finite.
func (c MeasuredSupplierLiabilityProxy) ExpectedSupplierLiabilityUSDPerVerifiedUnit() (float64, bool) {
	if c.SupplierUSDPerUnit <= 0 || math.IsNaN(c.SupplierUSDPerUnit) || math.IsInf(c.SupplierUSDPerUnit, 0) {
		return 0, false
	}
	return c.SupplierUSDPerUnit, true
}

// settledSupplierLiabilityFact is the exact task-bound liability written by
// verification settlement. The task's frozen economic_supplier_payout_usd is a
// ceiling: generative observed-output settlement can lawfully pay less, so it
// must never be used as an actual-liability fallback.
type settledSupplierLiabilityFact struct {
	TaskID       uuid.UUID
	AmountMicros int64
	Refusals     []string
}

func (f settledSupplierLiabilityFact) valid() bool {
	return len(f.Refusals) == 0
}

// supplierLiabilityQuerier is the read surface needed to prove both the task
// cohort and its immutable settlement from one database snapshot. pgx.Tx and
// *pgxpool.Pool both implement it; production measurement always supplies a
// read-only REPEATABLE READ transaction.
type supplierLiabilityQuerier interface {
	pgxQuerier
	ledgerExec
}

// Tests use this seam to commit a dispute after the cohort read and prove the
// subsequent money reads still observe the same repeatable snapshot. It is nil
// in production and every test restores it before returning.
var supplierLiabilityAfterCohortReadHook func()

// settledSupplierLiabilitiesForTasks validates the normal three-row task
// settlement before returning supplier money. A missing or malformed row is an
// authority refusal, not zero dollars and not a row to silently omit from the
// cohort. The ledger amounts must also equal the canonical observed-output
// settlement reconstructed from the task's frozen authority.
func (s *Store) settledSupplierLiabilitiesForTasks(
	ctx context.Context, q supplierLiabilityQuerier, taskIDs []uuid.UUID, currency string,
) (map[uuid.UUID]settledSupplierLiabilityFact, error) {
	out := make(map[uuid.UUID]settledSupplierLiabilityFact, len(taskIDs))
	if len(taskIDs) == 0 {
		return out, nil
	}
	for _, taskID := range taskIDs {
		out[taskID] = settledSupplierLiabilityFact{TaskID: taskID}
	}

	rows, err := q.Query(ctx, `
		WITH requested(task_id) AS (
		  SELECT unnest($1::uuid[])
		)
		SELECT r.task_id,
		       t.id IS NOT NULL,
		       j.currency,
		       COUNT(le.id) FILTER (WHERE le.kind='buyer_charge')::int,
		       COUNT(le.id) FILTER (WHERE le.kind='supplier_credit')::int,
		       COUNT(le.id) FILTER (WHERE le.kind='platform_take')::int,
		       COUNT(le.id) FILTER (WHERE le.kind='buyer_charge' AND le.currency=$2)::int,
		       COUNT(le.id) FILTER (WHERE le.kind='supplier_credit' AND le.currency=$2)::int,
		       COUNT(le.id) FILTER (WHERE le.kind='platform_take' AND le.currency=$2)::int,
		       COUNT(le.id) FILTER (
		         WHERE le.kind='buyer_charge' AND le.buyer_id=j.buyer_id
		           AND le.supplier_id IS NULL)::int,
		       COUNT(le.id) FILTER (
		         WHERE le.kind='supplier_credit'
		           AND le.supplier_id=t.execution_supplier_id
		           AND le.buyer_id IS NULL)::int,
		       COUNT(le.id) FILTER (
		         WHERE le.kind='platform_take' AND le.supplier_id IS NULL
		           AND le.buyer_id IS NULL)::int,
		       COUNT(le.id) FILTER (
		         WHERE le.kind='supplier_credit' AND
		               COALESCE(le.payout_status,'pending') IN
		               ('pending','held','awaiting_funding','ready','sending',
		                'outcome_unknown','carried','released','exported'))::int,
		       COUNT(le.id) FILTER (
		         WHERE le.kind NOT IN ('buyer_charge','supplier_credit','platform_take'))::int,
		       CASE WHEN COUNT(le.id) FILTER (WHERE le.kind='buyer_charge')=1
		         THEN (MIN(le.amount_usd) FILTER (WHERE le.kind='buyer_charge')*1000000)::bigint END,
		       CASE WHEN COUNT(le.id) FILTER (WHERE le.kind='supplier_credit')=1
		         THEN (MIN(le.amount_usd) FILTER (WHERE le.kind='supplier_credit')*1000000)::bigint END,
		       CASE WHEN COUNT(le.id) FILTER (WHERE le.kind='platform_take')=1
		         THEN (MIN(le.amount_usd) FILTER (WHERE le.kind='platform_take')*1000000)::bigint END
		  FROM requested r
		  LEFT JOIN tasks t ON t.id=r.task_id
		  LEFT JOIN jobs j ON j.id=t.job_id
		  LEFT JOIN ledger_entries le ON le.task_id=t.id
		 GROUP BY r.task_id,t.id,j.currency
		 ORDER BY r.task_id`, taskIDs, currency)
	if err != nil {
		return nil, err
	}

	type ledgerShape struct {
		taskID                                                        uuid.UUID
		taskExists                                                    bool
		jobCurrency                                                   *string
		buyerRows, supplierRows, platformRows                         int
		buyerCurrencyRows, supplierCurrencyRows, platformCurrencyRows int
		buyerIdentityRows, supplierIdentityRows, platformIdentityRows int
		supplierActiveLiabilityRows, adjustmentRows                   int
		buyerMicros, supplierMicros, platformMicros                   *int64
	}
	shapes := make(map[uuid.UUID]ledgerShape, len(taskIDs))
	for rows.Next() {
		var shape ledgerShape
		if err := rows.Scan(
			&shape.taskID, &shape.taskExists, &shape.jobCurrency,
			&shape.buyerRows, &shape.supplierRows, &shape.platformRows,
			&shape.buyerCurrencyRows, &shape.supplierCurrencyRows,
			&shape.platformCurrencyRows, &shape.buyerIdentityRows,
			&shape.supplierIdentityRows, &shape.platformIdentityRows,
			&shape.supplierActiveLiabilityRows, &shape.adjustmentRows,
			&shape.buyerMicros, &shape.supplierMicros, &shape.platformMicros,
		); err != nil {
			rows.Close()
			return nil, err
		}
		shapes[shape.taskID] = shape
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	for _, taskID := range taskIDs {
		fact := out[taskID]
		shape, ok := shapes[taskID]
		if !ok || !shape.taskExists {
			fact.Refusals = append(fact.Refusals, fmt.Sprintf(
				"exact supplier settlement unavailable for task %s: task authority is missing", taskID))
			out[taskID] = fact
			continue
		}
		if shape.jobCurrency == nil || *shape.jobCurrency != currency {
			fact.Refusals = append(fact.Refusals, fmt.Sprintf(
				"exact supplier settlement unavailable for task %s: job currency does not match scoped currency %s",
				taskID, currency))
		}
		for name, count := range map[string]int{
			"buyer_charge":    shape.buyerRows,
			"supplier_credit": shape.supplierRows,
			"platform_take":   shape.platformRows,
		} {
			if count != 1 {
				fact.Refusals = append(fact.Refusals, fmt.Sprintf(
					"exact supplier settlement unavailable for task %s: found %d %s ledger rows, want exactly one",
					taskID, count, name))
			}
		}
		if shape.supplierActiveLiabilityRows != 1 {
			fact.Refusals = append(fact.Refusals, fmt.Sprintf(
				"exact supplier settlement unavailable for task %s: supplier credit is not an active settled liability",
				taskID))
		}
		if shape.adjustmentRows != 0 {
			fact.Refusals = append(fact.Refusals, fmt.Sprintf(
				"exact supplier settlement unavailable for task %s: found %d clawback/refund/adjustment ledger rows",
				taskID, shape.adjustmentRows))
		}
		for name, count := range map[string]int{
			"buyer_charge":    shape.buyerCurrencyRows,
			"supplier_credit": shape.supplierCurrencyRows,
			"platform_take":   shape.platformCurrencyRows,
		} {
			if count != 1 {
				fact.Refusals = append(fact.Refusals, fmt.Sprintf(
					"exact supplier settlement unavailable for task %s: %s is not uniquely denominated in %s",
					taskID, name, currency))
			}
		}
		for name, count := range map[string]int{
			"buyer_charge":    shape.buyerIdentityRows,
			"supplier_credit": shape.supplierIdentityRows,
			"platform_take":   shape.platformIdentityRows,
		} {
			if count != 1 {
				fact.Refusals = append(fact.Refusals, fmt.Sprintf(
					"exact supplier settlement unavailable for task %s: %s actor identity is missing or mismatched",
					taskID, name))
			}
		}
		if shape.buyerMicros != nil && shape.supplierMicros != nil && shape.platformMicros != nil {
			if *shape.buyerMicros >= 0 || *shape.supplierMicros < 0 || *shape.platformMicros < 0 ||
				*shape.buyerMicros+*shape.supplierMicros+*shape.platformMicros != 0 {
				fact.Refusals = append(fact.Refusals, fmt.Sprintf(
					"exact supplier settlement unavailable for task %s: three-row ledger settlement is unbalanced or has invalid signs",
					taskID))
			}
		} else if shape.buyerRows == 1 && shape.supplierRows == 1 && shape.platformRows == 1 {
			// A one-row amount must never become a synthetic zero through SQL NULL
			// handling. This branch is defensive against an unrepresentable money
			// fact or driver decoding regression.
			fact.Refusals = append(fact.Refusals, fmt.Sprintf(
				"exact supplier settlement unavailable for task %s: ledger amount is NULL or unrepresentable", taskID))
		}

		if len(fact.Refusals) == 0 {
			settled, settleErr := loadObservedOutputSettlement(ctx, q, taskID)
			if settleErr != nil {
				fact.Refusals = append(fact.Refusals, fmt.Sprintf(
					"exact supplier settlement unavailable for task %s: canonical settlement reconstruction failed: %v",
					taskID, settleErr))
			} else {
				wantBuyer := -usdToMicros(settled.BilledCharge)
				wantSupplier := usdToMicros(settled.SupplierPayout)
				wantPlatform := -wantBuyer - wantSupplier
				if *shape.buyerMicros != wantBuyer || *shape.supplierMicros != wantSupplier ||
					*shape.platformMicros != wantPlatform {
					fact.Refusals = append(fact.Refusals, fmt.Sprintf(
						"exact supplier settlement unavailable for task %s: ledger settlement does not match canonical observed-output settlement",
						taskID))
				} else {
					fact.AmountMicros = *shape.supplierMicros
				}
			}
		}
		sort.Strings(fact.Refusals)
		out[taskID] = fact
	}
	return out, nil
}

// MeasuredSupplierLiabilityProxiesByHardware reads the supplier-liability proxy
// of every cell that has completed primary work for one workload and model,
// grouped by the hardware class it ran on and then by cell.
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
func (s *Store) MeasuredSupplierLiabilityProxiesByHardware(
	ctx context.Context, scope supplierLiabilityScope,
) (map[string]map[string]MeasuredSupplierLiabilityProxy, error) {
	if err := scope.validate(); err != nil {
		return nil, err
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{
		IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly,
	})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) // no-op after Commit

	rows, err := tx.Query(ctx, `
		WITH scoped_jobs AS (
		  SELECT j.id, rs.considered_cells, j.workload_decision,
		         j.compute_plan, j.pricing_decision
		    FROM jobs j
		    JOIN runtime_shadow_selections rs ON rs.job_id = j.id
		   WHERE j.job_type = $1
		     AND COALESCE(j.model_ref,'') = $2
		     AND j.tier = $3
		     AND j.currency = $4
		     AND j.pricing_decision #>> '{catalogue,schedule_sha256}' = $5
		     AND j.workload_decision ->> 'model_revision' = $6
		     AND j.workload_decision ->> 'latency_class' = $10
		     AND rs.runtime_matrix_sha256 = $7
		     AND rs.policy_revision = $8
		     AND rs.selection_policy = $9
		     AND rs.latency_class = $10
		     AND rs.decided_at >= $11 AND rs.decided_at <= $12
		), primary_tasks AS (
		  SELECT t.id AS task_id,
		         t.runtime_cell_id AS cell_id,
		         COALESCE(t.execution_hw_class,'')  AS hw_class,
		         COALESCE(t.execution_hardware_identity,'') AS hardware_identity,
		         COALESCE(t.runtime_id,'')          AS runtime_id,
		         COALESCE(t.execution_engine,'')    AS engine,
		         COALESCE(t.execution_build_hash,'') AS build_hash,
		         COALESCE(t.execution_build_identity_policy,'') AS build_identity_policy,
		         COALESCE(t.runtime_matrix_sha256,'') AS runtime_matrix_sha256,
		         COALESCE(t.expected_output_records,0) AS units,
		         t.reported_duration_ms             AS duration_ms,
		         COALESCE(t.retry_count,0)          AS retries,
		         t.verification_outcome             AS outcome,
		         jsonb_build_object(
		           'job_type_spec', j.workload_decision #> '{binding,job_type}',
		           'request_params', j.workload_decision #> '{binding,params}',
		           'parallelism', j.workload_decision->'parallelism',
		           'compute_plan_version', j.compute_plan->'version',
		           'input_records', j.compute_plan->'input_records',
		           'input_bytes', j.compute_plan->'input_bytes',
		           'settlement_input_units', j.compute_plan->'settlement_input_units',
		           'estimated_input_tokens', j.compute_plan->'estimated_input_tokens',
		           'estimated_output_tokens', j.compute_plan->'estimated_output_tokens',
		           'input_depth_profile', j.compute_plan->'input_depth_profile',
		           'split_size', j.compute_plan->'split_size',
		           'primary_tasks', j.compute_plan->'primary_tasks',
		           'redundancy_tasks', j.compute_plan->'redundancy_tasks',
		           'honeypot_tasks', j.compute_plan->'honeypot_tasks',
		           'total_initial_tasks', j.compute_plan->'total_initial_tasks',
		           'verification_class', j.compute_plan->'verification_class',
		           'billable_units', j.pricing_decision->'billable_units',
		           'expected_output_records', to_jsonb(t.expected_output_records),
		           'input_depth_band', to_jsonb(COALESCE(t.input_depth_band,''))
		         )::text AS geometry_signature,
		         (j.workload_decision IS NOT NULL
		          AND j.workload_decision #> '{binding,job_type}' IS NOT NULL
		          AND jsonb_typeof(j.workload_decision->'parallelism') = 'object'
		          AND j.compute_plan IS NOT NULL
		          AND j.pricing_decision IS NOT NULL
		          AND COALESCE(j.compute_plan->>'version','') <> ''
		          AND COALESCE(j.compute_plan->>'input_records','') <> ''
		          AND COALESCE(j.compute_plan->>'input_bytes','') <> ''
		          AND COALESCE(j.compute_plan->>'settlement_input_units','') <> ''
		          AND COALESCE(j.compute_plan->>'estimated_input_tokens','') <> ''
		          AND COALESCE(j.compute_plan->>'estimated_output_tokens','') <> ''
		          AND jsonb_typeof(j.compute_plan->'input_depth_profile') = 'object'
		          AND COALESCE(j.compute_plan->>'split_size','') <> ''
		          AND COALESCE(j.compute_plan->>'primary_tasks','') <> ''
		          AND COALESCE(j.compute_plan->>'redundancy_tasks','') <> ''
		          AND COALESCE(j.compute_plan->>'honeypot_tasks','') <> ''
		          AND COALESCE(j.compute_plan->>'total_initial_tasks','') <> ''
		          AND COALESCE(j.pricing_decision->>'billable_units','') <> ''
		          AND t.expected_output_records IS NOT NULL
		          AND COALESCE(t.input_depth_band,'') <> '') AS geometry_complete
		    FROM tasks t
		    JOIN scoped_jobs j ON j.id = t.job_id
		   WHERE t.status = 'complete'
		     AND t.runtime_cell_id IS NOT NULL
		     AND NOT COALESCE(t.is_honeypot,false)
		     AND NOT COALESCE(t.is_redundancy,false)
		     AND t.hedged_from IS NULL
		     AND t.reported_duration_ms IS NOT NULL
		     AND t.completed_at >= $11 AND t.completed_at <= $12
		     AND COALESCE(t.expected_output_records,0) > 0
		     AND COALESCE(t.execution_hw_class,'') = $15
		     AND COALESCE(t.execution_hardware_identity,'') = $18
		     AND t.runtime_matrix_sha256 = $7
		     AND ($16 = '' OR t.runtime_cell_id IN ($16,$17))
		     AND EXISTS (
		       SELECT 1 FROM jsonb_array_elements(j.considered_cells) candidate
		        WHERE candidate->>'cell_id' = t.runtime_cell_id
		          AND candidate->>'quality_tier' = $13
		          AND candidate->>'verification' = $14)
		), terminal_tasks AS (
		  -- Wider than primary_tasks on purpose: every primary task that reached a
		  -- terminal state ON this cell, including the ones that failed and
		  -- therefore have no duration, no units and no verification outcome. A
		  -- task that failed before it was ever claimed has no execution hardware
		  -- and is not this cell's failure, so it is excluded.
		  SELECT t.runtime_cell_id AS cell_id,
		         COALESCE(t.execution_hw_class,'') AS hw_class,
		         COALESCE(t.execution_hardware_identity,'') AS hardware_identity,
		         COALESCE(t.runtime_id,'') AS runtime_id,
		         COALESCE(t.execution_engine,'') AS engine,
		         COALESCE(t.execution_build_hash,'') AS build_hash,
		         COALESCE(t.execution_build_identity_policy,'') AS build_identity_policy,
		         COALESCE(t.runtime_matrix_sha256,'') AS runtime_matrix_sha256,
		         t.status
		    FROM tasks t
		    JOIN scoped_jobs j ON j.id = t.job_id
		   WHERE t.status IN ('complete','failed')
		     AND t.runtime_cell_id IS NOT NULL
		     AND COALESCE(t.execution_hw_class,'') <> ''
		     AND NOT COALESCE(t.is_honeypot,false)
		     AND NOT COALESCE(t.is_redundancy,false)
		     AND t.hedged_from IS NULL
		     AND COALESCE(t.completed_at,t.started_at,t.created_at) >= $11
		     AND COALESCE(t.completed_at,t.started_at,t.created_at) <= $12
		     AND COALESCE(t.execution_hw_class,'') = $15
		     AND COALESCE(t.execution_hardware_identity,'') = $18
		     AND t.runtime_matrix_sha256 = $7
		     AND ($16 = '' OR t.runtime_cell_id IN ($16,$17))
		     AND EXISTS (
		       SELECT 1 FROM jsonb_array_elements(j.considered_cells) candidate
		        WHERE candidate->>'cell_id' = t.runtime_cell_id
		          AND candidate->>'quality_tier' = $13
		          AND candidate->>'verification' = $14)
		), terminal_rollup AS (
		  SELECT hw_class, hardware_identity, cell_id,
		         COUNT(*)::int AS attempts,
		         COUNT(*) FILTER (WHERE status = 'failed')::int AS fails
		    FROM terminal_tasks GROUP BY hw_class, hardware_identity, cell_id
		), identity_rollup AS (
		  SELECT hw_class, hardware_identity, cell_id,
		         MIN(runtime_id) AS runtime_id,
		         MIN(engine) AS engine,
		         MIN(build_hash) AS build_hash,
		         MIN(build_identity_policy) AS build_identity_policy,
		         MIN(runtime_matrix_sha256) AS runtime_matrix_sha256,
		         COUNT(DISTINCT runtime_id)::int AS runtime_count,
		         COUNT(DISTINCT engine)::int AS engine_count,
		         COUNT(DISTINCT build_hash)::int AS build_count,
		         COUNT(DISTINCT build_identity_policy)::int AS build_policy_count,
		         COUNT(DISTINCT runtime_matrix_sha256)::int AS matrix_count
		    FROM terminal_tasks GROUP BY hw_class, hardware_identity, cell_id
		), geometry_rollup AS (
		  -- Payout is priced from billable input/output units while the displayed
		  -- proxy and latency divide by delivered outputs. Both ratios are real for
		  -- one task geometry, but a mix of shallow and deep inputs can make the cell
		  -- receiving deeper traffic look dearer and slower. Refuse the entire
		  -- comparison unless every candidate observation has one complete geometry.
		  SELECT COUNT(DISTINCT geometry_signature)::int AS geometry_count,
		         BOOL_AND(geometry_complete) AS geometry_complete,
		         MIN(geometry_signature) AS geometry_signature
		    FROM primary_tasks
		)
		SELECT p.hw_class, p.hardware_identity, p.cell_id,
		       i.runtime_id, i.engine, i.build_hash, i.build_identity_policy, i.runtime_matrix_sha256,
		       i.runtime_count, i.engine_count, i.build_count, i.build_policy_count, i.matrix_count,
		       COUNT(*)::int,
		       SUM(p.units)::bigint,
		       percentile_cont(0.5) WITHIN GROUP (
		         ORDER BY p.duration_ms::float8 / p.units::float8),
		       jsonb_agg(p.task_id::text ORDER BY p.task_id::text),
		       SUM(p.retries)::float8 / COUNT(*)::float8,
		       COUNT(*) FILTER (WHERE p.outcome IS NOT NULL)::int,
		       COUNT(*) FILTER (WHERE p.outcome IS NOT NULL AND p.outcome <> 'pass')::int,
		       COALESCE(r.attempts, 0), COALESCE(r.fails, 0),
		       g.geometry_count, COALESCE(g.geometry_complete,false),
		       COALESCE(g.geometry_signature,'')
		  FROM primary_tasks p
		  LEFT JOIN terminal_rollup r
		         ON r.hw_class = p.hw_class
		        AND r.hardware_identity = p.hardware_identity
		        AND r.cell_id = p.cell_id
		  JOIN identity_rollup i
		         ON i.hw_class = p.hw_class
		        AND i.hardware_identity = p.hardware_identity
		        AND i.cell_id = p.cell_id
		 CROSS JOIN geometry_rollup g
		 GROUP BY p.hw_class, p.hardware_identity, p.cell_id,
		          i.runtime_id, i.engine, i.build_hash, i.build_identity_policy,
		          i.runtime_matrix_sha256, i.runtime_count, i.engine_count,
		          i.build_count, i.build_policy_count, i.matrix_count, r.attempts, r.fails,
		          g.geometry_count, g.geometry_complete, g.geometry_signature
		 ORDER BY p.hw_class, p.cell_id`,
		scope.JobType, scope.ModelRef, scope.Tier, scope.Currency,
		scope.CatalogueScheduleSHA256, scope.ModelRevision,
		scope.RuntimeMatrixSHA256, scope.PolicyRevision, scope.SelectionPolicy,
		scope.LatencyClass, scope.ObservedAfter, scope.ObservedBefore,
		scope.QualityTier, scope.Verification, scope.HWClass,
		scope.IncumbentCell, scope.ChallengerCell, scope.HardwareIdentity)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string]map[string]MeasuredSupplierLiabilityProxy{}
	type pendingSettlement struct {
		hwClass string
		cellID  string
		taskIDs []uuid.UUID
	}
	var pending []pendingSettlement
	var allTaskIDs []uuid.UUID
	for rows.Next() {
		var c MeasuredSupplierLiabilityProxy
		var taskIDsJSON []byte
		var runtimeCount, engineCount, buildCount, buildPolicyCount, matrixCount int
		var geometryCount int
		var geometryComplete bool
		var geometrySignature string
		if err := rows.Scan(&c.HWClass, &c.HardwareIdentity, &c.CellID, &c.RuntimeID, &c.Engine,
			&c.ExecutionBuildHash, &c.ExecutionBuildIdentityPolicy, &c.RuntimeMatrixSHA256,
			&runtimeCount, &engineCount, &buildCount, &buildPolicyCount, &matrixCount, &c.Samples,
			&c.Units, &c.MedianMsPerUnit, &taskIDsJSON, &c.RetryRate,
			&c.VerificationSamples, &c.VerificationFails,
			&c.TerminalAttempts, &c.TerminalFails, &geometryCount,
			&geometryComplete, &geometrySignature); err != nil {
			return nil, err
		}
		var encodedTaskIDs []string
		if err := json.Unmarshal(taskIDsJSON, &encodedTaskIDs); err != nil {
			return nil, fmt.Errorf("decode supplier-liability task authority: %w", err)
		}
		taskIDs := make([]uuid.UUID, 0, len(encodedTaskIDs))
		for _, encoded := range encodedTaskIDs {
			taskID, err := uuid.Parse(encoded)
			if err != nil {
				return nil, fmt.Errorf("decode supplier-liability task id %q: %w", encoded, err)
			}
			taskIDs = append(taskIDs, taskID)
		}
		c.JobType, c.ModelRef, c.Currency = scope.JobType, scope.ModelRef, scope.Currency
		c.Measured = c.Samples >= minSupplierLiabilitySamples &&
			c.RetryRate == 0 &&
			c.VerificationSamples >= minSupplierLiabilitySamples &&
			c.VerificationFails == 0 &&
			c.TerminalAttempts >= minSupplierLiabilitySamples &&
			c.TerminalFails == 0
		for name, count := range map[string]int{
			"runtime_id": runtimeCount, "execution_engine": engineCount,
			"execution_build_hash":            buildCount,
			"execution_build_identity_policy": buildPolicyCount,
			"runtime_matrix_sha256":           matrixCount,
		} {
			if count != 1 {
				c.AuthorityRefusals = append(c.AuthorityRefusals, fmt.Sprintf(
					"exact %s authority unavailable: observed %d distinct values in the bounded scope",
					name, count))
			}
		}
		if c.RuntimeID == "" || c.Engine == "" || c.ExecutionBuildHash == "" ||
			!validCurrentEngineBuildIdentityPolicy(c.ExecutionBuildIdentityPolicy) ||
			c.HardwareIdentity != scope.HardwareIdentity ||
			c.RuntimeMatrixSHA256 != scope.RuntimeMatrixSHA256 {
			c.AuthorityRefusals = append(c.AuthorityRefusals,
				"runtime/build/hardware identity is incomplete or disagrees with the exact scoped authority")
		}
		if geometryCount != 1 {
			c.AuthorityRefusals = append(c.AuthorityRefusals, fmt.Sprintf(
				"exact input/task geometry unavailable: observed %d distinct geometries in the bounded comparison cohort",
				geometryCount))
		} else if !geometryComplete || geometrySignature == "" {
			c.AuthorityRefusals = append(c.AuthorityRefusals,
				"exact input/task geometry unavailable: frozen compute, billable-unit, output-record, or depth authority is incomplete")
		} else {
			geometryDigest := sha256.Sum256([]byte(geometrySignature))
			c.InputGeometrySHA256 = hex.EncodeToString(geometryDigest[:])
		}
		sort.Strings(c.AuthorityRefusals)
		if len(c.AuthorityRefusals) > 0 {
			c.Measured = false
		}
		c.SourceBinding = BindingBound
		c.UnknownPlatformCostComponents = unknownPlatformCostComponents()
		if out[c.HWClass] == nil {
			out[c.HWClass] = map[string]MeasuredSupplierLiabilityProxy{}
		}
		out[c.HWClass][c.CellID] = c
		pending = append(pending, pendingSettlement{
			hwClass: c.HWClass, cellID: c.CellID, taskIDs: taskIDs,
		})
		allTaskIDs = append(allTaskIDs, taskIDs...)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Canonical settlement reconstruction performs its own reads. Release this
	// result set first so the method also works with tightly bounded pools.
	rows.Close()
	if supplierLiabilityAfterCohortReadHook != nil {
		supplierLiabilityAfterCohortReadHook()
	}
	settlements, err := s.settledSupplierLiabilitiesForTasks(
		ctx, tx, allTaskIDs, scope.Currency)
	if err != nil {
		return nil, err
	}
	const maxDisplayedSettlementRefusals = 8
	for _, candidate := range pending {
		c := out[candidate.hwClass][candidate.cellID]
		var supplierMicros int64
		invalidTasks, displayed := 0, 0
		for _, taskID := range candidate.taskIDs {
			fact, ok := settlements[taskID]
			if !ok {
				fact = settledSupplierLiabilityFact{
					TaskID: taskID,
					Refusals: []string{fmt.Sprintf(
						"exact supplier settlement unavailable for task %s: settlement audit returned no fact",
						taskID)},
				}
			}
			if !fact.valid() {
				invalidTasks++
				for _, refusal := range fact.Refusals {
					if displayed >= maxDisplayedSettlementRefusals {
						break
					}
					c.AuthorityRefusals = append(c.AuthorityRefusals, refusal)
					displayed++
				}
				continue
			}
			if fact.AmountMicros > math.MaxInt64-supplierMicros {
				return nil, fmt.Errorf(
					"supplier-liability settlement total overflows for %s/%s",
					candidate.hwClass, candidate.cellID)
			}
			supplierMicros += fact.AmountMicros
		}
		if invalidTasks > 0 {
			c.AuthorityRefusals = append(c.AuthorityRefusals, fmt.Sprintf(
				"exact supplier settlement unavailable for %d of %d completed primary tasks",
				invalidTasks, len(candidate.taskIDs)))
			c.Measured = false
			c.SupplierUSDPerUnit = 0
		} else if c.Units > 0 {
			c.SupplierUSDPerUnit = microsToUSD(supplierMicros) / float64(c.Units)
			if supplierMicros <= 0 {
				c.Measured = false
			}
		}
		sort.Strings(c.AuthorityRefusals)
		out[candidate.hwClass][candidate.cellID] = c
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return out, nil
}

// MeasuredSupplierLiabilityProxies is one hardware class of the above.
func (s *Store) MeasuredSupplierLiabilityProxies(
	ctx context.Context, scope supplierLiabilityScope,
) (map[string]MeasuredSupplierLiabilityProxy, error) {
	byHW, err := s.MeasuredSupplierLiabilityProxiesByHardware(ctx, scope)
	if err != nil {
		return nil, err
	}
	if byHW[scope.HWClass] == nil {
		return map[string]MeasuredSupplierLiabilityProxy{}, nil
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
	byHW map[string]map[string]MeasuredSupplierLiabilityProxy, candidates []string,
) string {
	bestHW, bestCovered, bestSamples := "", 0, 0
	for _, hw := range sortedKeys(byHW) {
		covered, samples := 0, 0
		hardwareIdentity := ""
		exactIdentity := true
		for _, cell := range candidates {
			if _, ok := measuredSupplierLiability(byHW[hw], cell); !ok {
				continue
			}
			proxy := byHW[hw][cell]
			if proxy.HWClass != hw || !validCanonicalHardwareIdentity(proxy.HardwareIdentity) {
				exactIdentity = false
				break
			}
			if hardwareIdentity == "" {
				hardwareIdentity = proxy.HardwareIdentity
			} else if hardwareIdentity != proxy.HardwareIdentity {
				exactIdentity = false
				break
			}
			covered++
			samples += proxy.Samples
		}
		if !exactIdentity || covered < 2 {
			continue
		}
		if covered > bestCovered || (covered == bestCovered && samples > bestSamples) {
			bestHW, bestCovered, bestSamples = hw, covered, samples
		}
	}
	return bestHW
}

// SelectorLiabilityRegret compares one scope's routing decisions with the
// lowest measured supplier-liability proxy among eligible cells. It is not cost
// regret: unknown platform components prevent that claim.
//
// Decisions whose candidate set is not fully measured are COUNTED, not scored.
// Dropping them would let a scope with two measured decisions and four hundred
// unmeasured ones report a confident zero, which is the failure mode this whole
// file exists to avoid.
type SelectorLiabilityRegret struct {
	JobType          string `json:"job_type"`
	ModelRef         string `json:"model_ref"`
	HWClass          string `json:"hw_class"`
	HardwareIdentity string `json:"hardware_identity"`
	Currency         string `json:"currency"`

	Decisions           int `json:"decisions"`
	ScoredDecisions     int `json:"scored_decisions"`
	UnmeasuredDecisions int `json:"unmeasured_decisions"`
	DivergedDecisions   int `json:"diverged_decisions"`
	// ExactPairDecisions counts decisions that routed the named incumbent and
	// considered both the incumbent and challenger under this scope's quality,
	// verification, policy and epoch. ExactPairScoredDecisions is the subset for
	// which both exact-scope observations were measured. Promotion gates on the
	// latter; an unrelated A/B decision can never authorize C/D.
	ExactPairDecisions       int `json:"exact_pair_decisions,omitempty"`
	ExactPairScoredDecisions int `json:"exact_pair_scored_decisions,omitempty"`
	UnrelatedDecisions       int `json:"unrelated_decisions,omitempty"`

	// Liability regret is per verified unit, in settlement currency, of the
	// routed cell against the lowest measured supplier-liability proxy in the
	// same decision. It says nothing about total platform cost.
	TotalLiabilityRegretPerUnit float64 `json:"total_supplier_liability_regret_per_verified_unit"`
	MaxLiabilityRegretPerUnit   float64 `json:"max_supplier_liability_regret_per_verified_unit"`
	MeanLiabilityRegretPerUnit  float64 `json:"mean_supplier_liability_regret_per_verified_unit"`

	// LowestLiabilityCell is the cell with the lowest proxy in every scored
	// decision, or "" when the scored decisions do not agree on one.
	LowestLiabilityCell string `json:"lowest_measured_supplier_liability_cell"`

	// G053: per-phase latency regret terms. Same decision corpus as the
	// liability totals above — not a second learner. Each phase is scored only
	// when both a predicted and a realized duration exist for that phase on
	// the job/task; otherwise the decision is counted under
	// PhaseUnmeasuredDecisions and contributes nothing to the totals.
	//
	// Phase names match eta_calibration.phase / DecomposeTaskPhases. Realtime
	// and lease subjects are not yet joined into this batch-shadow query;
	// their phase actuals live on eta_calibration with subject_kind set and
	// are reportable via PhaseCalibrationRegret, not this struct.
	PhaseLatencyRegretMS map[string]PhaseLatencyRegretTerm `json:"phase_latency_regret_ms,omitempty"`
	// PhaseUnmeasuredDecisions counts decisions that had no per-phase
	// predicted+realized pair for any requested phase. Parallel to
	// UnmeasuredDecisions for liability.
	PhaseUnmeasuredDecisions int `json:"phase_unmeasured_decisions,omitempty"`
}

// PhaseLatencyRegretTerm is one phase's aggregated latency regret over scored
// decisions. Regret is realized_ms − predicted_ms (positive means slower than
// predicted). Unknown stays out of the aggregate entirely.
type PhaseLatencyRegretTerm struct {
	Phase           string  `json:"phase"`
	ScoredDecisions int     `json:"scored_decisions"`
	TotalRegretMS   float64 `json:"total_regret_ms"`
	MeanRegretMS    float64 `json:"mean_regret_ms"`
	MaxRegretMS     float64 `json:"max_regret_ms"`
}

// shadowDecisionRow is the part of a recorded decision regret needs.
type shadowDecisionRow struct {
	JobType    string
	ModelRef   string
	RoutedCell string
	ShadowCell string
	Considered []string
	Candidates []shadowCandidate
}

// SelectorLiabilityRegretForScope pairs recorded shadow decisions with measured
// supplier-liability proxies.
//
// hwClass is a parameter rather than read off the decision because a decision is
// recorded at admission, before any worker has been chosen: the hardware a job
// ran on is a property of the execution, and asking a decision what hardware it
// used would be asking it something it cannot know.
func (s *Store) SelectorLiabilityRegretForScope(
	ctx context.Context, scope supplierLiabilityScope,
) (SelectorLiabilityRegret, map[string]MeasuredSupplierLiabilityProxy, error) {
	if err := scope.validate(); err != nil {
		return SelectorLiabilityRegret{}, nil, err
	}
	liabilities, err := s.MeasuredSupplierLiabilityProxies(ctx, scope)
	if err != nil {
		return SelectorLiabilityRegret{}, nil, err
	}
	rows, err := s.pool.Query(ctx, `
		SELECT rs.job_type, rs.model_ref, rs.routed_cell_id, rs.shadow_cell_id,
		       rs.considered_cells
		  FROM runtime_shadow_selections rs
		  JOIN jobs j ON j.id = rs.job_id
		 WHERE rs.job_type = $1 AND rs.model_ref = $2
		   AND j.tier = $3 AND j.currency = $4
		   AND j.pricing_decision #>> '{catalogue,schedule_sha256}' = $5
		   AND j.workload_decision ->> 'model_revision' = $6
		   AND j.workload_decision ->> 'latency_class' = $10
		   AND rs.runtime_matrix_sha256 = $7
		   AND rs.policy_revision = $8
		   AND rs.selection_policy = $9
		   AND rs.latency_class = $10
		   AND rs.decided_at >= $11 AND rs.decided_at <= $12
		 ORDER BY rs.decided_at`,
		scope.JobType, scope.ModelRef, scope.Tier, scope.Currency,
		scope.CatalogueScheduleSHA256, scope.ModelRevision,
		scope.RuntimeMatrixSHA256, scope.PolicyRevision, scope.SelectionPolicy,
		scope.LatencyClass, scope.ObservedAfter, scope.ObservedBefore)
	if err != nil {
		return SelectorLiabilityRegret{}, nil, err
	}
	defer rows.Close()

	out := SelectorLiabilityRegret{
		JobType: scope.JobType, ModelRef: scope.ModelRef,
		HWClass: scope.HWClass, HardwareIdentity: scope.HardwareIdentity,
		Currency:             scope.Currency,
		PhaseLatencyRegretMS: map[string]PhaseLatencyRegretTerm{},
	}
	winners := map[string]int{}
	for rows.Next() {
		var d shadowDecisionRow
		var candidatesJSON []byte
		if err := rows.Scan(&d.JobType, &d.ModelRef, &d.RoutedCell, &d.ShadowCell, &candidatesJSON); err != nil {
			return SelectorLiabilityRegret{}, nil, err
		}
		if err := json.Unmarshal(candidatesJSON, &d.Candidates); err != nil {
			return SelectorLiabilityRegret{}, nil, fmt.Errorf("decode scoped shadow candidates: %w", err)
		}
		for _, candidate := range d.Candidates {
			if candidate.QualityTier == scope.QualityTier &&
				candidate.Verification == scope.Verification {
				d.Considered = append(d.Considered, candidate.CellID)
			}
		}
		out.Decisions++
		if d.RoutedCell != d.ShadowCell {
			out.DivergedDecisions++
		}
		var regret float64
		var lowest string
		var ok bool
		if scope.IncumbentCell != "" {
			if d.RoutedCell != scope.IncumbentCell ||
				!containsString(d.Considered, scope.IncumbentCell) ||
				!containsString(d.Considered, scope.ChallengerCell) {
				out.UnrelatedDecisions++
				continue
			}
			out.ExactPairDecisions++
			regret, lowest, ok = scoreExactPairLiabilityRegret(
				d, liabilities, scope.IncumbentCell, scope.ChallengerCell)
			if ok {
				out.ExactPairScoredDecisions++
			}
		} else {
			regret, lowest, ok = scoreDecisionLiabilityRegret(d, liabilities)
		}
		if !ok {
			out.UnmeasuredDecisions++
			continue
		}
		out.ScoredDecisions++
		out.TotalLiabilityRegretPerUnit += regret
		if regret > out.MaxLiabilityRegretPerUnit {
			out.MaxLiabilityRegretPerUnit = regret
		}
		winners[lowest]++
	}
	if err := rows.Err(); err != nil {
		return SelectorLiabilityRegret{}, nil, err
	}
	if out.ScoredDecisions > 0 {
		out.MeanLiabilityRegretPerUnit = out.TotalLiabilityRegretPerUnit / float64(out.ScoredDecisions)
	}
	if len(winners) == 1 {
		for cell := range winners {
			out.LowestLiabilityCell = cell
		}
	}
	// Attach per-phase latency terms from eta_calibration for jobs in scope.
	// A phase without both predicted_ms and realized_ms contributes nothing.
	if err := s.attachPhaseLatencyRegret(ctx, scope, &out); err != nil {
		return SelectorLiabilityRegret{}, nil, err
	}
	return out, liabilities, nil
}

// attachPhaseLatencyRegret fills PhaseLatencyRegretMS from eta_calibration
// rows that have BOTH predicted_ms and realized_ms for non-total phases,
// scoped the same way as the liability query. Missing prediction is counted
// under PhaseUnmeasuredDecisions, never treated as zero predicted.
func (s *Store) attachPhaseLatencyRegret(
	ctx context.Context, scope supplierLiabilityScope, out *SelectorLiabilityRegret,
) error {
	rows, err := s.pool.Query(ctx, `
		SELECT e.phase, e.predicted_ms, e.realized_ms
		  FROM eta_calibration e
		  JOIN jobs j ON j.id = e.job_id
		  JOIN runtime_shadow_selections rs ON rs.job_id = j.id
		 WHERE rs.job_type = $1 AND rs.model_ref = $2
		   AND j.tier = $3 AND j.currency = $4
		   AND j.pricing_decision #>> '{catalogue,schedule_sha256}' = $5
		   AND j.workload_decision ->> 'model_revision' = $6
		   AND j.workload_decision ->> 'latency_class' = $10
		   AND rs.runtime_matrix_sha256 = $7
		   AND rs.policy_revision = $8
		   AND rs.selection_policy = $9
		   AND rs.latency_class = $10
		   AND rs.decided_at >= $11 AND rs.decided_at <= $12
		   AND COALESCE(e.phase,'total') <> 'total'
		   AND e.realized_ms IS NOT NULL`,
		scope.JobType, scope.ModelRef, scope.Tier, scope.Currency,
		scope.CatalogueScheduleSHA256, scope.ModelRevision,
		scope.RuntimeMatrixSHA256, scope.PolicyRevision, scope.SelectionPolicy,
		scope.LatencyClass, scope.ObservedAfter, scope.ObservedBefore)
	if err != nil {
		return err
	}
	defer rows.Close()

	agg := map[string]*PhaseLatencyRegretTerm{}
	seen := 0
	unmeasured := 0
	for rows.Next() {
		var phase string
		var predicted, realized *float64
		if err := rows.Scan(&phase, &predicted, &realized); err != nil {
			return err
		}
		seen++
		if predicted == nil || realized == nil {
			unmeasured++
			continue
		}
		regret, ok := PhaseRegretMS(*predicted, *realized, true, true)
		if !ok {
			unmeasured++
			continue
		}
		t := agg[phase]
		if t == nil {
			t = &PhaseLatencyRegretTerm{Phase: phase}
			agg[phase] = t
		}
		t.ScoredDecisions++
		t.TotalRegretMS += regret
		if regret > t.MaxRegretMS {
			t.MaxRegretMS = regret
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	out.PhaseUnmeasuredDecisions = unmeasured
	if len(agg) == 0 {
		// No phase pairs in scope — leave map empty (omitempty) so a reader
		// does not mistake an empty object for measured zero regret.
		if seen == 0 {
			out.PhaseLatencyRegretMS = nil
		}
		return nil
	}
	out.PhaseLatencyRegretMS = make(map[string]PhaseLatencyRegretTerm, len(agg))
	for phase, t := range agg {
		if t.ScoredDecisions > 0 {
			t.MeanRegretMS = t.TotalRegretMS / float64(t.ScoredDecisions)
		}
		out.PhaseLatencyRegretMS[phase] = *t
	}
	return nil
}

// scoreExactPairLiabilityRegret scores only the pair a promotion names. Other
// considered cells remain part of the historical decision but cannot become a
// surrogate baseline for this receipt.
func scoreExactPairLiabilityRegret(
	d shadowDecisionRow,
	liabilities map[string]MeasuredSupplierLiabilityProxy,
	incumbent, challenger string,
) (regret float64, lowest string, ok bool) {
	if d.RoutedCell != incumbent || !containsString(d.Considered, incumbent) ||
		!containsString(d.Considered, challenger) {
		return 0, "", false
	}
	incumbentLiability, incumbentOK := measuredSupplierLiability(liabilities, incumbent)
	challengerLiability, challengerOK := measuredSupplierLiability(liabilities, challenger)
	if !incumbentOK || !challengerOK {
		return 0, "", false
	}
	lowest, best := incumbent, incumbentLiability
	if challengerLiability < best {
		lowest, best = challenger, challengerLiability
	}
	return incumbentLiability - best, lowest, true
}

// handleAdminSelectorLiabilityRegret exposes the same measured supplier-liability regret
// used by the promotion gate. It is read-only and admin-scoped: a public caller must
// not be able to turn a partially measured scope into a routing claim, and an
// operator needs the explicit unmeasured count to know when the report is not
// promotion evidence.
func (s *Server) handleAdminSelectorLiabilityRegret(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	value := func(name string) string { return strings.TrimSpace(query.Get(name)) }
	scope := supplierLiabilityScope{
		JobType: value("job_type"), ModelRef: value("model_ref"),
		HWClass: value("hw_class"), HardwareIdentity: value("hardware_identity"),
		Tier:          value("tier"),
		ModelRevision: value("model_revision"), QualityTier: value("quality_tier"),
		Verification: value("verification_contract"), LatencyClass: value("latency_class"),
	}
	missing := make([]string, 0, 8)
	for name, field := range map[string]string{
		"job_type": scope.JobType, "model_ref": scope.ModelRef, "hw_class": scope.HWClass,
		"hardware_identity": scope.HardwareIdentity,
		"tier":              scope.Tier, "model_revision": scope.ModelRevision,
		"quality_tier": scope.QualityTier, "verification_contract": scope.Verification,
		"latency_class": scope.LatencyClass,
	} {
		if field == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		writeErr(w, http.StatusBadRequest, "missing exact selector-liability scope: "+strings.Join(missing, ", "))
		return
	}
	now := time.Now().UTC()
	activation := currentActivation()
	scope.RuntimeMatrixSHA256 = generatedRuntimeMatrixSHA256
	scope.SelectionPolicy = shadowSelectionPolicy
	scope.PolicyRevision = activation.PolicyRevision
	scope.ObservedBefore = now
	scope.ObservedAfter = now.Add(-supplierLiabilityObservationWindow)
	currency, schedule, err := s.store.resolveSupplierLiabilityEconomicEpoch(r.Context(), scope)
	if err != nil {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	scope.Currency = currency
	scope.CatalogueScheduleSHA256 = schedule
	if err := scope.validate(); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	regret, liabilities, err := s.store.SelectorLiabilityRegretForScope(r.Context(), scope)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "selector regret unavailable")
		return
	}
	// Per-cell economics projections for the same measured rows. Selector
	// evidence only: not a PricingDecision and never freezes money. The proxy is
	// the eventual accepted payout with reliability as a separate eligibility
	// gate; platform costs remain unknown.
	var projections map[string]CellEconomicsProjection
	if catalogue, catalogueErr := s.store.LoadCataloguePriceAuthority(r.Context(), scope.ModelRef); catalogueErr == nil &&
		catalogue.ScheduleSHA256 == scope.CatalogueScheduleSHA256 &&
		catalogue.SettlementCurrency == scope.Currency {
		projections = ProjectCellEconomicsMap(liabilities, catalogue, scope.Tier)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"supplier_liability_regret":           regret,
		"measured_supplier_liability_proxies": liabilities,
		"cell_economics":                      projections,
		"pricing_authority":                   "PricingDecision remains the sole frozen money authority; cell_economics is a re-derivable selector projection",
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
		JobType:          value("job_type"),
		ModelRef:         value("model_ref"),
		ModelRevision:    value("model_revision"),
		Tier:             value("tier"),
		QualityTier:      value("quality_tier"),
		Verification:     value("verification_contract"),
		HWClass:          value("hw_class"),
		HardwareIdentity: value("hardware_identity"),
		LatencyClass:     value("latency_class"),
		RuntimeID:        value("runtime_id"),
		CellID:           value("cell_id"),
	}
	incumbent := value("incumbent_cell")
	missing := make([]string, 0, 11)
	for name, field := range map[string]string{
		"job_type":              scope.JobType,
		"model_ref":             scope.ModelRef,
		"model_revision":        scope.ModelRevision,
		"tier":                  scope.Tier,
		"quality_tier":          scope.QualityTier,
		"verification_contract": scope.Verification,
		"hw_class":              scope.HWClass,
		"hardware_identity":     scope.HardwareIdentity,
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
	recorded, err := s.store.RecordCellPromotionEvaluation(r.Context(), evidence)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "selector promotion receipt could not be recorded")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"passed":                evidence.Passed(),
		"evidence":              evidence,
		"evidence_sha256":       digest,
		"promotion_receipt_ref": receiptRef,
		"evidence_recorded":     recorded,
		"activation_applied":    false,
	})
}

// selectorRollbackRequest is deliberately smaller than ActivationPolicyEntry.
// An operator may name only the immutable target revision and an audit note;
// the store reconstructs the complete policy from that revision and writes the
// rollback forward. Accepting lifecycle or receipt fields here would create a
// second activation authority beside the append-only policy table.
type selectorRollbackRequest struct {
	TargetPolicyRevision int64  `json:"target_policy_revision"`
	Note                 string `json:"note"`
}

// selectorActivationRequest applies a reviewed activation-policy write. The
// entries are the same shape ApplyActivationPolicy already validates: capability
// digests, lifecycle rules, and the promotion-gate receipt for routable states.
// This route is the production caller that was missing beside rollback.
type selectorActivationRequest struct {
	Entries []ActivationPolicyEntry `json:"entries"`
	Note    string                  `json:"note"`
}

// handleAdminSelectorActivation is the production caller for
// Store.ApplyActivationPolicy. Evaluation (GET promotion) remains separate and
// never writes policy; this route is the governed apply step after review.
func (s *Server) handleAdminSelectorActivation(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		writeErr(w, http.StatusInternalServerError, "selector activation unavailable")
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 64<<10+1))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "reading selector activation request: "+err.Error())
		return
	}
	if len(raw) > 64<<10 {
		writeErr(w, http.StatusRequestEntityTooLarge, "selector activation request exceeds 65536 bytes")
		return
	}
	var request selectorActivationRequest
	if err := decodeStrictJSONObject(raw, &request); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid selector activation json: "+err.Error())
		return
	}
	request.Note = strings.TrimSpace(request.Note)
	if len(request.Entries) == 0 {
		writeErr(w, http.StatusBadRequest, "entries must name at least one activation policy statement")
		return
	}
	if request.Note == "" || len(request.Note) > 512 || strings.ContainsAny(request.Note, "\r\n\t") {
		writeErr(w, http.StatusBadRequest, "note must be a non-empty single-line value no longer than 512 bytes")
		return
	}
	revision, err := s.store.ApplyActivationPolicy(r.Context(), request.Entries, request.Note)
	if err != nil {
		writeErr(w, http.StatusConflict, "selector activation refused: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"activation_applied": true,
		"policy_revision":    revision,
	})
}

// adminDirectedJobRequest submits a job onto a named directed cell. The cell id
// is never taken from the nested job body: a buyer cannot reach this route, and
// the operator argument stays outside the buyer's request shape.
type adminDirectedJobRequest struct {
	BuyerID        string    `json:"buyer_id"`
	DirectedCellID string    `json:"directed_cell_id"`
	IdempotencyKey string    `json:"idempotency_key"`
	Job            jobSubmit `json:"job"`
}

// handleAdminDirectedJob is the production entry point for directed routing.
// It freezes the named cell via buildWorkloadDecisionDirected and submits
// through the ordinary money path so challenger cells can accumulate the
// primary-task samples promotion requires.
func (s *Server) handleAdminDirectedJob(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		writeErr(w, http.StatusInternalServerError, "directed job submission unavailable")
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxJobSubmitBodyBytes+1))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "reading directed job request: "+err.Error())
		return
	}
	if len(raw) > maxJobSubmitBodyBytes {
		writeErr(w, http.StatusRequestEntityTooLarge,
			fmt.Sprintf("directed job request exceeds the %d byte submission limit", maxJobSubmitBodyBytes))
		return
	}
	var request adminDirectedJobRequest
	if err := decodeStrictJSONObject(raw, &request); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid directed job json: "+err.Error())
		return
	}
	request.DirectedCellID = strings.TrimSpace(request.DirectedCellID)
	if request.DirectedCellID == "" {
		writeErr(w, http.StatusBadRequest, "directed_cell_id is required")
		return
	}
	buyerID, err := uuid.Parse(strings.TrimSpace(request.BuyerID))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "buyer_id must be a UUID")
		return
	}
	idempotencyKey := strings.TrimSpace(request.IdempotencyKey)
	if !idempotencyKeyPattern.MatchString(idempotencyKey) {
		writeErr(w, http.StatusBadRequest, "idempotency_key is required and must be 8-128 characters using letters, digits, '.', '_', ':', or '-'")
		return
	}
	// Digest the nested job the way buyer submit digests its body, so idempotent
	// replays of the same directed request collide correctly with each other.
	jobBlob, err := json.Marshal(request.Job)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "encoding directed job body")
		return
	}
	digest := sha256.Sum256(jobBlob)
	sub := request.Job
	sub.IdempotencyKey = idempotencyKey
	sub.RequestSHA256 = hex.EncodeToString(digest[:])
	sub.directedCellID = request.DirectedCellID
	resp, herr := s.createJob(r.Context(), buyerID, sub)
	if herr != nil {
		writeErr(w, herr.status, herr.msg)
		return
	}
	if resp.WebhookSecret != "" {
		setSecretResponseHeaders(w)
	}
	if resp.IdempotentReplay {
		w.Header().Set("Idempotent-Replayed", "true")
	}
	writeJSON(w, http.StatusAccepted, resp)
}

// handleAdminSelectorRollback is the production caller for the existing
// append-only rollback authority. It never deletes or edits a promotion: the
// store writes a new revision naming the target, so an operator can audit both
// the promotion and the reversal after the fact.
func (s *Server) handleAdminSelectorRollback(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		writeErr(w, http.StatusInternalServerError, "selector rollback unavailable")
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 8<<10+1))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "reading selector rollback request: "+err.Error())
		return
	}
	if len(raw) > 8<<10 {
		writeErr(w, http.StatusRequestEntityTooLarge, "selector rollback request exceeds 8192 bytes")
		return
	}
	var request selectorRollbackRequest
	if err := decodeStrictJSONObject(raw, &request); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid selector rollback json: "+err.Error())
		return
	}
	request.Note = strings.TrimSpace(request.Note)
	if request.TargetPolicyRevision <= 0 {
		writeErr(w, http.StatusBadRequest, "target_policy_revision must be positive")
		return
	}
	if request.Note == "" || len(request.Note) > 512 || strings.ContainsAny(request.Note, "\r\n\t") {
		writeErr(w, http.StatusBadRequest, "note must be a non-empty single-line value no longer than 512 bytes")
		return
	}
	revision, err := s.store.RollbackActivationPolicy(r.Context(), request.TargetPolicyRevision, request.Note)
	if err != nil {
		writeErr(w, http.StatusConflict, "selector rollback refused: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"activation_applied":       true,
		"policy_revision":          revision,
		"rollback_target_revision": request.TargetPolicyRevision,
	})
}

// scoreDecisionLiabilityRegret returns the routed cell's supplier-liability
// regret against the lowest measured proxy it was considered against.
//
// ok is false unless the routed cell AND at least one other considered cell are
// measured. A decision scored against a candidate set where only the winner has
// data would always report zero regret, which is not evidence that the winner
// was right — it is evidence that nothing else was tried.
func scoreDecisionLiabilityRegret(
	d shadowDecisionRow, liabilities map[string]MeasuredSupplierLiabilityProxy,
) (regret float64, lowest string, ok bool) {
	routed, hasRouted := measuredSupplierLiability(liabilities, d.RoutedCell)
	if !hasRouted {
		return 0, "", false
	}
	best, bestCell, others := routed, d.RoutedCell, 0
	for _, cell := range d.Considered {
		if cell == d.RoutedCell {
			continue
		}
		liability, has := measuredSupplierLiability(liabilities, cell)
		if !has {
			continue
		}
		others++
		if liability < best {
			best, bestCell = liability, cell
		}
	}
	if others == 0 {
		return 0, "", false
	}
	return routed - best, bestCell, true
}

// measuredSupplierLiability is the accepted supplier payout of a cell, or false
// when its independent reliability evidence is absent, under-sampled, retried,
// rejected, or terminally failed. Until a governed reliability policy defines
// an acceptable retry threshold, any nonzero (including invalid) RetryRate is a
// refusal. Keeping this gate separate from the money accessor is essential:
// failures make a cell ineligible; they do not create a fictional supplier
// payment.
func eligibleMeasuredSupplierLiability(c MeasuredSupplierLiabilityProxy) (float64, bool) {
	if !c.Measured ||
		len(c.AuthorityRefusals) != 0 ||
		(c.SourceBinding == BindingBound && strings.TrimSpace(c.Currency) == "") ||
		c.Samples < minSupplierLiabilitySamples ||
		c.RetryRate != 0 ||
		c.VerificationSamples < minSupplierLiabilitySamples ||
		c.VerificationFails != 0 ||
		c.TerminalAttempts < minSupplierLiabilitySamples ||
		c.TerminalFails != 0 {
		return 0, false
	}
	return c.ExpectedSupplierLiabilityUSDPerVerifiedUnit()
}

func measuredSupplierLiability(liabilities map[string]MeasuredSupplierLiabilityProxy, cell string) (float64, bool) {
	c, ok := liabilities[cell]
	if !ok {
		return 0, false
	}
	return eligibleMeasuredSupplierLiability(c)
}

// rankCellsByMeasuredSupplierLiability orders cell ids from lowest to highest
// measured supplier-liability proxy, dropping unmeasured cells. This helper
// does not authorize a cost-based selection while platform costs are unknown.
func rankCellsByMeasuredSupplierLiability(liabilities map[string]MeasuredSupplierLiabilityProxy, cells []string) []string {
	type scored struct {
		cell string
		cost float64
	}
	var ranked []scored
	for _, cell := range cells {
		liability, ok := measuredSupplierLiability(liabilities, cell)
		if !ok {
			continue
		}
		ranked = append(ranked, scored{cell, liability})
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
