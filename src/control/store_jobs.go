package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"reflect"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Split out of store.go, which had grown to 5,727 lines across roughly two
// dozen unrelated responsibilities.  Same package, same behaviour: this is a
// file move so that a reviewer can hold one subject at a time and two people
// can edit payouts and job submission without conflicting.

func (s *Store) JobOutputRef(ctx context.Context, jobID uuid.UUID) (string, error) {
	var ref string
	err := s.pool.QueryRow(ctx, `SELECT COALESCE(output_ref,'') FROM jobs WHERE id=$1`, jobID).Scan(&ref)
	return ref, err
}

func (s *Store) JobBuyerID(ctx context.Context, jobID uuid.UUID) (uuid.UUID, error) {
	var buyerID uuid.UUID
	err := s.pool.QueryRow(ctx, `SELECT buyer_id FROM jobs WHERE id=$1`, jobID).Scan(&buyerID)
	return buyerID, err
}

func (s *Store) DeleteOldJobEvents(ctx context.Context, before time.Time) (int64, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM job_events WHERE created_at < $1`, before)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// DeleteOldAlphaRequests removes non-accepted alpha leads created before the
// cutoff. Accepted rows are retained (they have no buyer account to tombstone).

type AdminJob struct {
	ID           uuid.UUID `json:"id"`
	BuyerID      uuid.UUID `json:"buyer_id"`
	Status       string    `json:"status"`
	JobType      string    `json:"job_type"`
	ModelRef     string    `json:"model_ref"`
	Tier         string    `json:"tier"`
	TaskCount    int       `json:"task_count"`
	TasksDone    int       `json:"tasks_done"`
	EstimatedUSD float64   `json:"estimated_usd"`
	ActualUSD    float64   `json:"actual_usd"`
	CreatedAt    time.Time `json:"created_at"`
}

type BuyerJobSummary struct {
	ID           uuid.UUID `json:"id"`
	Status       string    `json:"status"`
	JobType      string    `json:"job_type"`
	ModelRef     string    `json:"model_ref"`
	Tier         string    `json:"tier"`
	TaskCount    int       `json:"task_count"`
	TasksDone    int       `json:"tasks_done"`
	EstimatedUSD float64   `json:"estimated_usd"`
	ActualUSD    float64   `json:"actual_usd"`
	BudgetState  string    `json:"budget_state"`
	ChargeStatus string    `json:"charge_status"`
	CreatedAt    time.Time `json:"created_at"`
}

func (s *Store) ListJobsForBuyer(ctx context.Context, buyerID uuid.UUID, limit int) ([]BuyerJobSummary, error) {
	if limit <= 0 || limit > 200 {
		limit = 100
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id,status,job_type,COALESCE(model_ref,''),tier,
		       COALESCE(task_count,0),COALESCE(tasks_done,0),
		       COALESCE(estimated_usd,0)::float8,COALESCE(actual_usd,0)::float8,
		       COALESCE(budget_state,''),COALESCE(charge_status,''),created_at
		  FROM jobs
		 WHERE buyer_id=$1
		 ORDER BY created_at DESC,id DESC
		 LIMIT $2`, buyerID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]BuyerJobSummary, 0)
	for rows.Next() {
		var job BuyerJobSummary
		if err := rows.Scan(&job.ID, &job.Status, &job.JobType, &job.ModelRef, &job.Tier,
			&job.TaskCount, &job.TasksDone, &job.EstimatedUSD, &job.ActualUSD,
			&job.BudgetState, &job.ChargeStatus, &job.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, job)
	}
	return out, rows.Err()
}

func (s *Store) ListJobsAdmin(ctx context.Context) ([]AdminJob, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, buyer_id, status, job_type, COALESCE(model_ref,''), tier,
		        COALESCE(task_count,0), COALESCE(tasks_done,0),
		        COALESCE(estimated_usd,0), COALESCE(actual_usd,0), created_at
		 FROM jobs ORDER BY created_at DESC LIMIT 200`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AdminJob
	for rows.Next() {
		var a AdminJob
		if err := rows.Scan(&a.ID, &a.BuyerID, &a.Status, &a.JobType, &a.ModelRef,
			&a.Tier, &a.TaskCount, &a.TasksDone, &a.EstimatedUSD, &a.ActualUSD, &a.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func validateJobClaimAuthority(j *jobRow) error {
	if j == nil {
		return errors.New("job placement authority is missing")
	}
	if err := validatePlacementRequirement(j.PlacementRequirement, j.WorkloadDecision); err != nil {
		return err
	}
	binding := j.WorkloadDecision.Binding
	hardwareMatches := sameStrings(j.HWClasses, binding.Constraints.HWClasses)
	if j.PlacementRequirement.Version >= 2 {
		hardwareMatches = sameStrings(j.HWClasses, j.PlacementRequirement.HWClasses)
	}
	if j.JobType != j.WorkloadDecision.RuntimeJobType ||
		j.JobType != binding.JobType.Type ||
		j.ModelRef != binding.Model.Ref ||
		j.Tier != binding.Tier ||
		math.Abs(float64(j.MinMemoryGB)-j.WorkloadDecision.MinimumMemoryGB) > 0.000001 ||
		!hardwareMatches ||
		!sameStrings(j.DataResidency, binding.Constraints.DataResidency) ||
		j.MinReputation != binding.MinReputation ||
		j.MaxDurationSecs != binding.Constraints.MaxDurationSecs ||
		j.OfferedRateUsdHr != j.PlacementRequirement.OfferedRateUsdHr ||
		math.IsNaN(float64(j.OfferedRateUsdHr)) ||
		math.IsInf(float64(j.OfferedRateUsdHr), 0) ||
		j.OfferedRateUsdHr < 0 {
		return errors.New("job claim columns conflict with the frozen workload authority")
	}
	return nil
}

func (s *Store) SubmitJobTx(ctx context.Context, j *jobRow, tasks []taskRow) error {
	if j == nil {
		return errors.New("cannot submit a nil job")
	}
	// CopyFrom persists the task's own job_id. Prove the entire obligation set
	// belongs to this new job before validating or reserving anything; otherwise
	// a direct caller could create a funded job/reserve while attaching its paid
	// tasks to an unrelated existing job.
	for _, task := range tasks {
		if task.JobID != j.ID {
			return fmt.Errorf("task %s belongs to job %s, want submitted job %s",
				task.ID, task.JobID, j.ID)
		}
	}
	activationRevision := activationAdmissionRevision(j.activationPolicyRevision)
	hasWebhook := j.WebhookID != uuid.Nil || j.WebhookURL != "" || j.WebhookSigningSecretSealed != ""
	if hasWebhook && (j.WebhookID == uuid.Nil || j.WebhookURL == "" ||
		!strings.HasPrefix(j.WebhookSigningSecretSealed, "enc:")) {
		return errors.New("job webhook requires id, url, and encrypted signing secret together")
	}
	if err := ValidateWorkloadDecisionSnapshot(j.WorkloadDecision); err != nil {
		return fmt.Errorf("refusing job without valid workload decision: %w", err)
	}
	if err := validateCurrentPlacementRequirement(
		j.PlacementRequirement, j.WorkloadDecision,
	); err != nil {
		return fmt.Errorf("%w: refusing new job without current placement authority: %v",
			errQuotePhysicalAuthorityUnavailable, err)
	}
	if err := validateJobClaimAuthority(j); err != nil {
		return fmt.Errorf("refusing job without consistent claim authority: %w", err)
	}
	if err := ValidateEconomicPlanSnapshot(j.EconomicPlan); err != nil {
		return fmt.Errorf("refusing job without valid economic plan: %w", err)
	}
	jobCurrency := j.EconomicPlan.Schedule.Currency
	if err := RequireSettlementCurrency(jobCurrency); err != nil {
		return fmt.Errorf("refusing job outside the settlement currency: %w", err)
	}
	if err := ValidateComputePlanEconomicSnapshot(j.ComputePlan, j.WorkloadDecision, j.EconomicPlan); err != nil {
		return fmt.Errorf("refusing job without valid compute plan: %w", err)
	}
	if err := ValidateDistributedPricingDecisionSnapshotWithStore(
		ctx, s, j.PricingDecision, j.WorkloadDecision, j.ComputePlan,
		j.PlacementRequirement, j.EconomicPlan,
	); err != nil {
		return fmt.Errorf("refusing job without valid composite pricing authority: %w", err)
	}
	if j.EconomicPlan.Input.InitialTaskCount != len(tasks) {
		return fmt.Errorf("economic plan initial_task_count=%d does not match %d initial tasks",
			j.EconomicPlan.Input.InitialTaskCount, len(tasks))
	}
	if j.TaskCount != len(tasks) {
		return fmt.Errorf("job task_count=%d does not match %d initial tasks", j.TaskCount, len(tasks))
	}
	var primaryTasks, redundancyTasks, honeypotTasks int
	for _, task := range tasks {
		if task.InputDepthBand != "" && !validInputDepthBand(task.InputDepthBand) {
			return fmt.Errorf("task %s has invalid input depth band %q", task.ID, task.InputDepthBand)
		}
		if j.EconomicInputSource == economicInputSourceSubmitStream && !task.IsHoneypot &&
			!validInputDepthBand(task.InputDepthBand) {
			return fmt.Errorf("streamed task %s lacks frozen input depth authority", task.ID)
		}
		switch {
		case task.IsHoneypot:
			honeypotTasks++
		case task.IsRedundancy:
			redundancyTasks++
		default:
			primaryTasks++
		}
	}
	if primaryTasks != j.ComputePlan.PrimaryTasks ||
		redundancyTasks != j.ComputePlan.RedundancyTasks ||
		honeypotTasks != j.ComputePlan.HoneypotTasks {
		return fmt.Errorf(
			"initial task classes primary=%d redundancy=%d honeypot=%d do not match compute plan %d/%d/%d",
			primaryTasks, redundancyTasks, honeypotTasks,
			j.ComputePlan.PrimaryTasks, j.ComputePlan.RedundancyTasks, j.ComputePlan.HoneypotTasks,
		)
	}
	if err := validateCurrentUniformTaskEconomicAuthority(
		j.WorkloadDecision, j.ComputePlan, j.EconomicPlan, j.PricingDecision, tasks,
	); err != nil {
		return fmt.Errorf("%w: refusing new job without exact task-economic authority: %w",
			errQuotePhysicalAuthorityUnavailable, err)
	}
	if j.PricingDecision.TaskEconomicPolicy == uniformSinglePrimaryTaskEconomicsV1 &&
		j.EconomicInputSource != economicInputSourceSubmitStream {
		return fmt.Errorf("%w: refusing current task economics without exact submit-stream input authority",
			errQuotePhysicalAuthorityUnavailable)
	}
	// Independence is enforced at claim (linked suppliers filtered) and at
	// verification settlement (same-supplier / no-independent-supplier fails
	// closed). Submit does not require an independent supplier to already be
	// online — the queue may wait.
	if j.SplitSize != j.ComputePlan.SplitSize {
		return fmt.Errorf("job split_size=%d does not match compute plan %d", j.SplitSize, j.ComputePlan.SplitSize)
	}
	if math.Abs(float64(j.MinMemoryGB)-j.ComputePlan.MinimumMemoryGB) > 0.000001 {
		return fmt.Errorf("job minimum memory %.6f does not match compute plan %.6f",
			j.MinMemoryGB, j.ComputePlan.MinimumMemoryGB)
	}
	if j.ETASecs != j.ComputePlan.ETAP50Secs {
		return fmt.Errorf("job ETA %d does not match compute plan p50 %d", j.ETASecs, j.ComputePlan.ETAP50Secs)
	}
	if j.ETARawSecs <= 0 || j.ETARawSecs > j.ETASecs {
		return fmt.Errorf("job raw ETA %d is not within calibrated ETA %d", j.ETARawSecs, j.ETASecs)
	}
	if j.EconomicInputRecords != int64(j.ComputePlan.InputRecords) ||
		j.EconomicInputBytes != j.ComputePlan.InputBytes {
		return fmt.Errorf("job input authority records=%d bytes=%d does not match compute plan %d/%d",
			j.EconomicInputRecords, j.EconomicInputBytes,
			j.ComputePlan.InputRecords, j.ComputePlan.InputBytes)
	}
	if j.EconomicInputSource == economicInputSourceSubmitStream {
		var primaryRecords int64
		for _, task := range tasks {
			if task.ExpectedOutputRecords <= 0 {
				return fmt.Errorf("exact streamed job task %s lacks expected output record authority", task.ID)
			}
			if !task.IsHoneypot && !task.IsRedundancy {
				primaryRecords += task.ExpectedOutputRecords
			}
		}
		if primaryRecords != j.EconomicInputRecords {
			return fmt.Errorf("primary task output records %d do not match exact streamed input records %d",
				primaryRecords, j.EconomicInputRecords)
		}
	}
	// A task may not be labelled with a class its own flags contradict. The
	// database enforces this too; here it gets a message that names the task.
	for _, task := range tasks {
		if task.VerificationClass == "" {
			continue
		}
		if err := validateTaskVerificationClass(
			task.VerificationClass, task.IsHoneypot, task.IsRedundancy); err != nil {
			return fmt.Errorf("task %s: %w", task.ID, err)
		}
	}
	if math.Abs(j.EstimatedUSD-j.EconomicPlan.InitialBuyerChargeUSD) > 0.000001 {
		return fmt.Errorf("job estimate %.6f does not match frozen economic charge %.6f",
			j.EstimatedUSD, j.EconomicPlan.InitialBuyerChargeUSD)
	}
	if math.Abs(j.SLAPremiumUSD-j.EconomicPlan.Input.SLAPremiumUSD) > 0.000001 {
		return fmt.Errorf("job SLA premium %.6f does not match frozen economic premium %.6f",
			j.SLAPremiumUSD, j.EconomicPlan.Input.SLAPremiumUSD)
	}
	if j.FirmQuote {
		if j.FirmQuoteMaxUSD <= 0 {
			return errors.New("firm quote requires a positive job-level buyer ceiling")
		}
		// Pre-PricingDecision direct rows retained the cap inside EconomicPlan.
		// New quote-bound submissions deliberately keep that plan identical to the
		// reviewed quote and carry the acceptance cap only on the job. Preserve the
		// old direct-row invariant where there is no quote to bind it instead of
		// forcing a second plan/decision for new work.
		if j.QuoteID == uuid.Nil &&
			math.Abs(j.FirmQuoteMaxUSD-j.EconomicPlan.Input.FirmQuoteMaxUSD) > 0.000001 {
			return fmt.Errorf("firm quote max %.6f does not match frozen economic cap %.6f",
				j.FirmQuoteMaxUSD, j.EconomicPlan.Input.FirmQuoteMaxUSD)
		}
	} else if j.EconomicPlan.Input.FirmQuoteMaxUSD > 0 || j.FirmQuoteMaxUSD > 0 {
		return errors.New("non-firm job cannot carry a firm economic cap")
	}
	planJSON, err := json.Marshal(j.EconomicPlan)
	if err != nil {
		return fmt.Errorf("marshal economic plan: %w", err)
	}
	workloadJSON, err := json.Marshal(j.WorkloadDecision)
	if err != nil {
		return fmt.Errorf("marshal workload decision: %w", err)
	}
	workloadSHA256, err := workloadDecisionDigest(j.WorkloadDecision)
	if err != nil {
		return fmt.Errorf("hash workload decision: %w", err)
	}
	computeJSON, err := json.Marshal(j.ComputePlan)
	if err != nil {
		return fmt.Errorf("marshal compute plan: %w", err)
	}
	computeSHA256, err := computePlanDigest(j.ComputePlan)
	if err != nil {
		return fmt.Errorf("hash compute plan: %w", err)
	}
	placementJSON, err := json.Marshal(j.PlacementRequirement)
	if err != nil {
		return fmt.Errorf("marshal placement requirement: %w", err)
	}
	placementSHA256, err := placementRequirementDigest(j.PlacementRequirement)
	if err != nil {
		return fmt.Errorf("hash placement requirement: %w", err)
	}
	pricingJSON, err := json.Marshal(j.PricingDecision)
	if err != nil {
		return fmt.Errorf("marshal pricing decision: %w", err)
	}
	pricingSHA256, err := pricingDecisionDigest(j.PricingDecision)
	if err != nil {
		return fmt.Errorf("hash pricing decision: %w", err)
	}
	// TopologyDecision is frozen inside the accept transaction — never only as
	// post-commit shadow TopologyPlan. Batch today chooses independent fan-out
	// over FabricUnknown (POOL) and records LOCAL_CLUSTER as construction-refused.
	topologyDecision, err := buildBatchTopologyDecision(j.WorkloadDecision)
	if err != nil {
		return fmt.Errorf("refusing job without valid topology decision: %w", err)
	}
	topologyJSON, err := json.Marshal(topologyDecision)
	if err != nil {
		return fmt.Errorf("marshal topology decision: %w", err)
	}
	topologySHA256, err := topologyDecisionDigest(topologyDecision)
	if err != nil {
		return fmt.Errorf("hash topology decision: %w", err)
	}
	if err := ValidateTopologyDecisionDigest(topologyDecision, topologySHA256); err != nil {
		return fmt.Errorf("refusing job with inconsistent topology decision: %w", err)
	}
	j.TopologyDecision = topologyDecision
	// RuntimeDecision freezes the lifecycle-ranked (or directed) singleton as
	// accepted authority in the same transaction. Shadow measured re-ranking
	// stays post-commit and non-authoritative (Step 8).
	runtimeDecision, err := buildBatchRuntimeDecision(
		j.WorkloadDecision, j.PlacementRequirement, activationRevision,
	)
	if err != nil {
		return fmt.Errorf("refusing job without valid runtime decision: %w", err)
	}
	runtimeJSON, err := json.Marshal(runtimeDecision)
	if err != nil {
		return fmt.Errorf("marshal runtime decision: %w", err)
	}
	runtimeSHA256, err := runtimeDecisionDigest(runtimeDecision)
	if err != nil {
		return fmt.Errorf("hash runtime decision: %w", err)
	}
	if err := ValidateRuntimeDecisionDigest(runtimeDecision, runtimeSHA256); err != nil {
		return fmt.Errorf("refusing job with inconsistent runtime decision: %w", err)
	}
	j.RuntimeDecision = runtimeDecision
	// Step 9: batch accept cannot bind a worker (pull + SKIP LOCKED). Record
	// PENDING_CLAIM so the fourth meaning of placement is explicit rather than
	// silently absent. The claim path binds BOUND with claim-time eligibility.
	if _, err := newBatchAcceptPendingWorkerPlacement(); err != nil {
		return fmt.Errorf("refusing job without valid pending worker placement: %w", err)
	}
	// EvidenceEnvelope cites the digests frozen below; it does not re-hash
	// decision bodies. Built before the TX so a seal failure never opens a
	// transaction, then written in the same TX as the jobs INSERT.
	envelope, err := buildBatchAcceptEvidenceEnvelope(j.ID, batchAcceptBoundDigests{
		RequestSHA256:     j.SubmitRequestSHA256,
		WorkloadSHA256:    workloadSHA256,
		PlacementSHA256:   placementSHA256,
		PricingSHA256:     pricingSHA256,
		RuntimeSHA256:     runtimeSHA256,
		TopologySHA256:    topologySHA256,
		ComputePlanSHA256: computeSHA256,
	})
	if err != nil {
		return fmt.Errorf("build evidence envelope: %w", err)
	}
	envelopeJSON, err := json.Marshal(envelope)
	if err != nil {
		return fmt.Errorf("marshal evidence envelope: %w", err)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("%w: begin job admission: %v",
			errActivationAdmissionUnavailable, err)
	}
	defer tx.Rollback(ctx)
	if err := guardActivationAdmissionTx(ctx, tx, activationRevision); err != nil {
		return err
	}
	if durableAdmissionPhysicalRecheckHook != nil {
		durableAdmissionPhysicalRecheckHook()
	}
	if err := validateCurrentPlacementRequirement(
		j.PlacementRequirement, j.WorkloadDecision,
	); err != nil {
		return fmt.Errorf("%w: refusing job at durable placement ingress: %v",
			errQuotePhysicalAuthorityUnavailable, err)
	}
	if j.QuoteID == uuid.Nil {
		if err := validateCurrentCataloguePriceAuthorityFrom(ctx, tx, j.PricingDecision.Catalogue); err != nil {
			return fmt.Errorf("%w: refusing unquoted job at durable ingress: %v",
				errCataloguePhysicalAuthorityUnavailable, err)
		}
	} else if err := validateBoundCataloguePriceAuthorityFrom(
		ctx, tx, j.PricingDecision.Catalogue,
	); err != nil {
		return fmt.Errorf("%w: refusing bound job without current physical authority: %v",
			errCataloguePhysicalAuthorityUnavailable, err)
	}
	if err := validateCurrentPlacementCataloguePhysicalAuthority(
		j.PlacementRequirement, j.PricingDecision.Catalogue,
	); err != nil {
		return fmt.Errorf("%w: refusing job whose price does not bind its placement: %v",
			errCataloguePhysicalAuthorityUnavailable, err)
	}
	if j.QuoteID == uuid.Nil {
		if err := validateCurrentCatalogueModelPointerFrom(
			ctx, tx, j.PricingDecision.Catalogue,
		); err != nil {
			return fmt.Errorf("%w: refusing unquoted job after catalogue pointer changed: %v",
				errCataloguePhysicalAuthorityUnavailable, err)
		}
	} else if err := validateCurrentCataloguePhysicalPointerFrom(
		ctx, tx, j.PricingDecision.Catalogue,
	); err != nil {
		return fmt.Errorf("%w: refusing bound job after physical catalogue authority changed: %v",
			errCataloguePhysicalAuthorityUnavailable, err)
	}
	if j.PrepaidRequired {
		if err := reservePrepaidForJobTx(ctx, tx, j.BuyerID,
			usdToMicros(j.EconomicPlan.ReservedBuyerChargeUSD)); err != nil {
			return err
		}
	} else {
		// Free-credit (and non-prepaid) admits: serialise against the same buyer
		// funding lock realtime uses, and hold the reserved ceiling — not the
		// initial estimate — so concurrent free-credit batch jobs cannot each
		// observe the full grant. Saved-PM deferred collection skips inside.
		if err := reserveFreeCreditForJobTx(ctx, tx, j.BuyerID,
			usdToMicros(j.EconomicPlan.ReservedBuyerChargeUSD)); err != nil {
			return err
		}
	}
	if j.QuoteID != uuid.Nil {
		var quoteComputeSHA256, quoteCurrency, quotePlacementSHA256, quotePricingSHA256 string
		var quoteETARawSecs int
		var quotePricingJSON []byte
		var quoteExpired bool
		if err := tx.QueryRow(ctx,
			`SELECT COALESCE(compute_plan_sha256,''),currency,
			        COALESCE(placement_requirement_sha256,''),
			        COALESCE(pricing_decision_sha256,''),pricing_decision,
			        COALESCE(eta_p50_secs_raw,0),
			        COALESCE(expires_at <= clock_timestamp(),true)
			   FROM quotes
			  WHERE id=$1 AND buyer_id=$2
			  FOR SHARE`,
			j.QuoteID, j.BuyerID,
		).Scan(
			&quoteComputeSHA256, &quoteCurrency, &quotePlacementSHA256,
			&quotePricingSHA256, &quotePricingJSON, &quoteETARawSecs, &quoteExpired,
		); err != nil {
			return fmt.Errorf("load bound quote compute authority: %w", err)
		}
		// GetBindableQuote is an advisory preflight performed before the buyer's
		// input stream is consumed. The firm promise must still be alive at the
		// durable acceptance boundary: otherwise a client can begin before TTL,
		// stall through expiry, and persist old money after the quote is no longer
		// bindable. clock_timestamp(), rather than transaction-start now(), makes
		// this check observe time after the activation/physical locks above.
		if quoteExpired {
			return fmt.Errorf("%w: request and review a new quote", errBoundQuoteExpired)
		}
		if quoteComputeSHA256 == "" || quoteComputeSHA256 != computeSHA256 {
			return errors.New("job compute plan does not match its bound quote")
		}
		if quoteCurrency != jobCurrency {
			return fmt.Errorf("%w: job currency %s does not match bound quote currency %s",
				errCurrencyMismatch, jobCurrency, quoteCurrency)
		}
		if quotePlacementSHA256 == "" || quotePlacementSHA256 != placementSHA256 {
			return errors.New("job offered rate and placement requirement do not match its bound quote")
		}
		if quoteETARawSecs <= 0 || quoteETARawSecs != j.ETARawSecs {
			return errors.New("job raw ETA does not match its bound quote")
		}
		var quotePricing PricingDecision
		if err := json.Unmarshal(quotePricingJSON, &quotePricing); err != nil {
			return fmt.Errorf("decode bound quote pricing authority: %w", err)
		}
		if quotePricingSHA256 == "" || pricingSHA256 != quotePricingSHA256 ||
			quotePricing.ComputePlanSHA256 != j.PricingDecision.ComputePlanSHA256 ||
			quotePricing.PlacementRequirementSHA256 != placementSHA256 ||
			quotePricing.WorkloadDecisionSHA256 != j.PricingDecision.WorkloadDecisionSHA256 ||
			!reflect.DeepEqual(quotePricing.Catalogue, j.PricingDecision.Catalogue) {
			return errors.New("job pricing decision does not exactly match its bound quote")
		}
		if j.FirmQuote && math.Abs(j.FirmQuoteMaxUSD-quotePricing.MaximumBuyerPrice) > 0.000001 {
			return fmt.Errorf("firm quote max %.6f does not match bound PricingDecision ceiling %.6f",
				j.FirmQuoteMaxUSD, quotePricing.MaximumBuyerPrice)
		}
	}
	if j.ProjectOrderID != uuid.Nil || j.ProjectStepID != "" {
		if j.PricingDecision.FixedPoint == nil {
			return errors.New("project job has no fixed-point PricingDecision authority")
		}
		if err := reserveProjectStepTx(ctx, tx, j, jobCurrency,
			j.PricingDecision.FixedPoint.AcceptedCeilingNanos, pricingSHA256); err != nil {
			return err
		}
	}

	var economicInputRecords, economicInputBytes, economicInputSource any
	if j.EconomicInputSource != "" {
		economicInputRecords = j.EconomicInputRecords
		economicInputBytes = j.EconomicInputBytes
		economicInputSource = j.EconomicInputSource
	}
	_, err = tx.Exec(ctx,
		`INSERT INTO jobs
		   (id, buyer_id, status, job_type, model_ref, input_ref, output_ref,
		    tier, verification_policy, estimated_usd, actual_usd, task_count, tasks_done,
		    min_memory_gb, max_duration_secs, hw_classes, data_residency, job_type_spec, split_size,
		    offered_rate_usd_hr, eta_secs, eta_secs_raw, max_usd, budget_state, quote_id, min_reputation,
		    deadline_secs, firm_quote, firm_quote_max_usd, sla_guarantee_secs, sla_premium_usd,
		    economic_input_records, economic_input_bytes, economic_input_source,
		    submit_idempotency_key, submit_request_sha256, prefix_id,
		    workload_decision, workload_decision_sha256,
		    compute_plan, compute_plan_sha256,
		    placement_requirement, placement_requirement_sha256,
		    pricing_decision, pricing_decision_sha256,
		    runtime_decision, runtime_decision_sha256,
		    topology_decision, topology_decision_sha256,
		    currency, prepaid_required,
		    project_order_id, project_step_id, evidence_envelope_sha256)
		 VALUES ($1,$2,'queued',$3,$4,$5,$6,$7,$8,$9,0,$10,0,
		         $11,$12,$13,$14,$15,$16,$17,$18,$19,$20,'tracking',$21,$22,$23,$24,$25,$26,$27,
		         $28,$29,$30,NULLIF($31,''),NULLIF($32,''),NULLIF($33,''),
		         $34,$35,$36,$37,$38,$39,$40,$41,$42,$43,$44,$45,$46,$47,$48,NULLIF($49,''),$50)`,
		j.ID, j.BuyerID, j.JobType, j.ModelRef, j.InputRef, j.OutputRef,
		j.Tier, j.VerificationPolicy, j.EstimatedUSD, j.TaskCount,
		j.MinMemoryGB, j.MaxDurationSecs, nullStrSlice(j.HWClasses), nullStrSlice(j.DataResidency),
		nullJSON(j.JobTypeSpec), j.SplitSize, j.OfferedRateUsdHr, j.ETASecs, j.ETARawSecs,
		nullPosFloat(j.MaxUSD), nullUUID(j.QuoteID), j.MinReputation,
		j.DeadlineSecs, j.FirmQuote, nullPosFloat(j.FirmQuoteMaxUSD),
		nullPosInt(j.SLAGuaranteeSecs), nullPosFloat(j.SLAPremiumUSD),
		economicInputRecords, economicInputBytes, economicInputSource,
		j.SubmitIdempotencyKey, j.SubmitRequestSHA256, j.PrefixID,
		workloadJSON, workloadSHA256, computeJSON, computeSHA256,
		placementJSON, placementSHA256, pricingJSON, pricingSHA256,
		runtimeJSON, runtimeSHA256, topologyJSON, topologySHA256,
		jobCurrency, j.PrepaidRequired,
		nullUUID(j.ProjectOrderID), j.ProjectStepID, envelope.EnvelopeSHA256,
	)
	if err != nil {
		return err
	}
	if err := insertEvidenceEnvelopeTx(ctx, tx, envelope, envelopeJSON); err != nil {
		return err
	}
	if j.ProjectOrderID != uuid.Nil {
		if _, err := tx.Exec(ctx, `
			INSERT INTO project_order_steps
			  (project_order_id,step_id,job_id,quote_id,pricing_decision_sha256,accepted_ceiling_nanos)
			VALUES ($1,$2,$3,$4,$5,$6)`,
			j.ProjectOrderID, j.ProjectStepID, j.ID, j.QuoteID, pricingSHA256,
			j.PricingDecision.FixedPoint.AcceptedCeilingNanos); err != nil {
			return fmt.Errorf("record project step reservation: %w", err)
		}
	}
	// Prefix chain is recorded in the same transaction as the job so a
	// claimable job never exists without the routing hint the claim SQL
	// reads. Empty chain is a no-op (short/cold inputs).
	for _, e := range j.PrefixChain {
		if !ValidPrefixID(e.PrefixID) || e.Depth <= 0 {
			continue
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO job_prefix_chain (job_id, depth, prefix_id)
			VALUES ($1,$2,$3)
			ON CONFLICT (job_id, depth) DO UPDATE SET prefix_id = EXCLUDED.prefix_id`,
			j.ID, e.Depth, e.PrefixID); err != nil {
			return fmt.Errorf("recording prefix chain for job %s: %w", j.ID, err)
		}
	}
	if hasWebhook {
		if _, err := tx.Exec(ctx, `
			INSERT INTO webhooks (id,buyer_id,job_id,url,signing_secret_sealed)
			VALUES ($1,$2,$3,$4,$5)`,
			j.WebhookID, j.BuyerID, j.ID, j.WebhookURL, j.WebhookSigningSecretSealed); err != nil {
			return fmt.Errorf("insert job webhook: %w", err)
		}
	}
	exactNanos := j.EconomicPlan.EconomicRoundingPolicy == economicRoundingPolicy &&
		j.EconomicPlan.BuyerChargePerTaskNanos > 0 &&
		j.EconomicPlan.SupplierPayoutPerTaskNanos > 0
	if _, err := tx.Exec(ctx, `
		INSERT INTO job_economic_plans (
		  job_id,plan_version,schedule_version,currency,plan_json,initial_task_count,
		  buyer_charge_per_task_usd,supplier_payout_per_task_usd,
		  initial_buyer_charge_usd,reserved_buyer_charge_usd,sla_premium_usd,firm_quote_max_usd,
		  buyer_charge_per_task_nanos,supplier_payout_per_task_nanos
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)`,
		j.ID, j.EconomicPlan.Version, j.EconomicPlan.Schedule.Version, jobCurrency, planJSON,
		j.EconomicPlan.Input.InitialTaskCount, j.EconomicPlan.BuyerChargePerTaskUSD,
		j.EconomicPlan.SupplierPayoutPerTaskUSD, j.EconomicPlan.InitialBuyerChargeUSD,
		j.EconomicPlan.ReservedBuyerChargeUSD, j.EconomicPlan.Input.SLAPremiumUSD,
		nullPosFloat(j.EconomicPlan.Input.FirmQuoteMaxUSD),
		nullEconomicNanos(j.EconomicPlan.BuyerChargePerTaskNanos, exactNanos),
		nullEconomicNanos(j.EconomicPlan.SupplierPayoutPerTaskNanos, exactNanos)); err != nil {
		return fmt.Errorf("insert economic plan: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO job_economic_reserves (job_id,reserved_tasks,consumed_tasks)
		VALUES ($1,$2,0)`, j.ID, j.EconomicPlan.Input.ExtraTaskReserve); err != nil {
		return fmt.Errorf("insert economic reserve: %w", err)
	}

	if len(tasks) > 0 {
		now := time.Now()
		_, err = tx.CopyFrom(ctx,
			pgx.Identifier{"tasks"},
			[]string{"id", "job_id", "status", "is_honeypot", "is_redundancy", "retry_count",
				"input_ref", "input_sha256", "input_depth_band", "result_key", "chunk_index", "expected_output_records", "visible_at",
				"economic_buyer_charge_usd", "economic_supplier_payout_usd",
				"economic_buyer_charge_nanos", "economic_supplier_payout_nanos",
				"verification_class", "verification_class_policy"},
			pgx.CopyFromSlice(len(tasks), func(i int) ([]any, error) {
				t := tasks[i]
				class := t.VerificationClass
				if class == "" {
					class = deriveTaskVerificationClass(
						j.ComputePlan.VerificationClass, t.IsHoneypot, t.IsRedundancy)
				}
				return []any{t.ID, t.JobID, "queued", t.IsHoneypot, t.IsRedundancy, int16(0),
					t.InputRef, nullSHA256Hex(t.InputSHA256), nullInputDepthBand(t.InputDepthBand), t.ResultKey, t.ChunkIndex, nullPosInt64(t.ExpectedOutputRecords), now,
					j.EconomicPlan.BuyerChargePerTaskUSD, j.EconomicPlan.SupplierPayoutPerTaskUSD,
					nullEconomicNanos(j.EconomicPlan.BuyerChargePerTaskNanos, exactNanos),
					nullEconomicNanos(j.EconomicPlan.SupplierPayoutPerTaskNanos, exactNanos),
					class, verificationClassPolicyRevision}, nil
			}),
		)
		if err != nil {
			return fmt.Errorf("copy tasks: %w", err)
		}
	}
	return tx.Commit(ctx)
}

type jobRow struct {
	ID                 uuid.UUID
	BuyerID            uuid.UUID
	JobType            string
	ModelRef           string
	InputRef           string
	OutputRef          string
	Tier               string
	VerificationPolicy []byte // jsonb
	EstimatedUSD       float64
	TaskCount          int
	MinMemoryGB        float32
	MaxDurationSecs    uint32
	HWClasses          []string // nil = any class
	DataResidency      []string // nil = unrestricted
	JobTypeSpec        []byte   // jsonb: the full submitted JobType (tag + fields)
	SplitSize          int
	OfferedRateUsdHr   float32
	ETASecs            int
	ETARawSecs         int
	MaxUSD             float64   // buyer hard spend cap (Budget Governor); 0 = no cap
	QuoteID            uuid.UUID // advisory quote bound to this job (Plane D D7); zero = none -> persisted NULL
	MinReputation      float32   // Elite-supplier gate: claim only by suppliers with reputation >= this (0 = any)
	DeadlineSecs       int       // watchdog policy: -1 opt out, 0 default, 60..604800 explicit wall-clock deadline
	FirmQuote          bool
	FirmQuoteMaxUSD    float64
	ProjectOrderID     uuid.UUID // zero = an ordinary, non-project job
	ProjectStepID      string
	SLAGuaranteeSecs   int
	SLAPremiumUSD      float64
	// PrepaidRequired freezes default-mode batch funding at acceptance. It is
	// false for legacy and explicitly deferred-charge jobs.
	PrepaidRequired      bool
	EconomicInputRecords int64
	EconomicInputBytes   int64
	EconomicInputSource  string
	EconomicPlan         EconomicPlan
	WorkloadDecision     WorkloadDecision
	ComputePlan          ComputePlan
	PlacementRequirement PlacementRequirement
	PricingDecision      PricingDecision
	// TopologyDecision is frozen at SubmitJobTx accept. Zero value means the
	// caller did not pre-build one; SubmitJobTx always writes a decision.
	TopologyDecision TopologyDecision
	// RuntimeDecision is frozen at SubmitJobTx accept: lifecycle-ranked (or
	// directed) cell, benchmark authority, and activation revision. Zero value
	// means the caller did not pre-build one; SubmitJobTx always writes it.
	RuntimeDecision            RuntimeDecision
	WebhookID                  uuid.UUID
	WebhookURL                 string
	WebhookSigningSecretSealed string
	SubmitIdempotencyKey       string
	SubmitRequestSHA256        string
	// PrefixID is the shallowest chain node (or empty). Advisory routing
	// hint only; never selects model, runtime or price.
	PrefixID string
	// PrefixChain is the nested trie recorded into job_prefix_chain at
	// submit. Empty when the input is shorter than the shallowest depth.
	PrefixChain []PrefixChainEntry
	// activationPolicyRevision is the epoch refreshed before this new job was
	// classified. It is a transaction guard at write time; Step 8 also freezes
	// the same epoch onto RuntimeDecision.ActivationPolicyRevision so replay
	// cites it as accepted authority, not only admit-guard context.
	activationPolicyRevision int64
}

type jobComputeAuthorityInputs struct {
	workloadBlob      []byte
	workloadSHA256    string
	computePlanBlob   []byte
	computePlanSHA256 string
	placementBlob     []byte
	placementSHA256   string
	pricingBlob       []byte
	pricingSHA256     string
	economicPlanBlob  []byte
}

// loadJobComputeAuthorityInputs reads the immutable execution and pricing
// authority in one row. These documents are persisted together for a job;
// splitting their reads forces every receipt path to pay a network round-trip
// per dependency and makes it easier to accidentally compare authorities from
// different snapshots.
func (s *Store) loadJobComputeAuthorityInputs(
	ctx context.Context,
	jobID uuid.UUID,
) (jobComputeAuthorityInputs, error) {
	var inputs jobComputeAuthorityInputs
	err := s.pool.QueryRow(ctx, `
		SELECT j.workload_decision,
		       COALESCE(j.workload_decision_sha256,''),
		       j.compute_plan,
		       COALESCE(j.compute_plan_sha256,''),
		       j.placement_requirement,
		       COALESCE(j.placement_requirement_sha256,''),
		       j.pricing_decision,
		       COALESCE(j.pricing_decision_sha256,''),
		       ep.plan_json
		  FROM jobs j
		  LEFT JOIN job_economic_plans ep ON ep.job_id=j.id
		 WHERE j.id=$1`, jobID).Scan(
		&inputs.workloadBlob, &inputs.workloadSHA256,
		&inputs.computePlanBlob, &inputs.computePlanSHA256,
		&inputs.placementBlob, &inputs.placementSHA256,
		&inputs.pricingBlob, &inputs.pricingSHA256,
		&inputs.economicPlanBlob,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return jobComputeAuthorityInputs{}, errNotFound
	}
	if err != nil {
		return jobComputeAuthorityInputs{}, err
	}
	return inputs, nil
}

func decodeJobWorkloadDecision(
	jobID uuid.UUID,
	blob []byte,
	frozenSHA256 string,
) (*WorkloadDecision, error) {
	if len(blob) == 0 {
		// Legacy jobs predate workload-decision authority. Their receipts stay
		// readable but do not invent a classification after execution.
		return nil, nil
	}
	var decision WorkloadDecision
	if err := json.Unmarshal(blob, &decision); err != nil {
		return nil, fmt.Errorf("decode workload decision for job %s: %w", jobID, err)
	}
	if err := ValidateFrozenWorkloadDecisionSnapshot(decision); err != nil {
		return nil, fmt.Errorf("invalid frozen workload decision for job %s: %w", jobID, err)
	}
	gotSHA256, err := workloadDecisionDigest(decision)
	if err != nil {
		return nil, fmt.Errorf("hash frozen workload decision for job %s: %w", jobID, err)
	}
	if frozenSHA256 == "" || frozenSHA256 != gotSHA256 {
		return nil, fmt.Errorf("frozen workload decision digest mismatch for job %s", jobID)
	}
	return &decision, nil
}

func decodeJobComputePlan(
	jobID uuid.UUID,
	blob []byte,
	frozenSHA256 string,
	workload *WorkloadDecision,
	economicBlob []byte,
) (*ComputePlan, error) {
	if len(blob) == 0 {
		// Legacy jobs remain readable without inventing execution geometry.
		return nil, nil
	}
	var plan ComputePlan
	if err := json.Unmarshal(blob, &plan); err != nil {
		return nil, fmt.Errorf("decode compute plan for job %s: %w", jobID, err)
	}
	if workload == nil {
		return nil, fmt.Errorf("job %s has a compute plan without workload authority", jobID)
	}
	if err := ValidateFrozenComputePlanSnapshot(plan, *workload); err != nil {
		return nil, fmt.Errorf("invalid frozen compute plan for job %s: %w", jobID, err)
	}
	if plan.ExecutionMode == computeExecutionDistributed {
		if len(economicBlob) == 0 {
			return nil, fmt.Errorf(
				"load economic authority for compute plan on job %s: %w",
				jobID, pgx.ErrNoRows,
			)
		}
		var economic EconomicPlan
		if err := json.Unmarshal(economicBlob, &economic); err != nil {
			return nil, fmt.Errorf("decode economic authority for compute plan on job %s: %w", jobID, err)
		}
		if err := ValidateComputePlanEconomicSnapshot(plan, *workload, economic); err != nil {
			return nil, fmt.Errorf("compute/economic authority mismatch for job %s: %w", jobID, err)
		}
	}
	gotSHA256, err := computePlanDigest(plan)
	if err != nil {
		return nil, fmt.Errorf("hash frozen compute plan for job %s: %w", jobID, err)
	}
	if frozenSHA256 == "" || frozenSHA256 != gotSHA256 {
		return nil, fmt.Errorf("frozen compute plan digest mismatch for job %s", jobID)
	}
	return &plan, nil
}

func decodeJobPlacementRequirement(
	jobID uuid.UUID,
	blob []byte,
	frozenSHA256 string,
	workload *WorkloadDecision,
) (*PlacementRequirement, error) {
	if len(blob) == 0 {
		// Exact reuse has no physical placement. Legacy physical jobs also
		// remain explicitly unverifiable instead of receiving a reconstruction.
		return nil, nil
	}
	var placement PlacementRequirement
	if err := json.Unmarshal(blob, &placement); err != nil {
		return nil, fmt.Errorf("decode placement requirement for job %s: %w", jobID, err)
	}
	if workload == nil {
		return nil, fmt.Errorf("job %s has placement without workload authority", jobID)
	}
	if err := validatePlacementRequirement(placement, *workload); err != nil {
		return nil, fmt.Errorf("invalid placement requirement for job %s: %w", jobID, err)
	}
	gotSHA256, err := placementRequirementDigest(placement)
	if err != nil {
		return nil, err
	}
	if frozenSHA256 == "" || frozenSHA256 != gotSHA256 {
		return nil, fmt.Errorf("frozen placement requirement digest mismatch for job %s", jobID)
	}
	return &placement, nil
}

func (s *Store) decodeJobPricingDecision(
	ctx context.Context,
	jobID uuid.UUID,
	blob []byte,
	frozenSHA256 string,
	placementBlob []byte,
	placementSHA256 string,
	workload *WorkloadDecision,
	compute *ComputePlan,
	economicBlob []byte,
) (*PricingDecision, error) {
	if len(blob) == 0 {
		return nil, nil
	}
	var pricing PricingDecision
	if err := json.Unmarshal(blob, &pricing); err != nil {
		return nil, fmt.Errorf("decode pricing decision for job %s: %w", jobID, err)
	}
	if workload == nil || compute == nil {
		return nil, fmt.Errorf("job %s pricing lacks workload or compute authority", jobID)
	}
	switch pricing.ExecutionMode {
	case computeExecutionDistributed:
		placement, err := decodeJobPlacementRequirement(
			jobID, placementBlob, placementSHA256, workload,
		)
		if err != nil {
			return nil, err
		}
		if placement == nil {
			return nil, fmt.Errorf("job %s physical pricing lacks placement authority", jobID)
		}
		if len(economicBlob) == 0 {
			return nil, fmt.Errorf(
				"load economic plan for pricing on job %s: %w",
				jobID, pgx.ErrNoRows,
			)
		}
		var economic EconomicPlan
		if err := json.Unmarshal(economicBlob, &economic); err != nil {
			return nil, fmt.Errorf("decode economic plan for pricing on job %s: %w", jobID, err)
		}
		if err := ValidateDistributedPricingDecisionSnapshotWithStore(
			ctx, s, pricing, *workload, *compute, *placement, economic,
		); err != nil {
			return nil, fmt.Errorf("invalid physical pricing decision for job %s: %w", jobID, err)
		}
	case computeExecutionExactReuse:
		if err := ValidateExactReusePricingDecisionSnapshot(
			pricing, *workload, *compute,
		); err != nil {
			return nil, fmt.Errorf("invalid exact-reuse pricing decision for job %s: %w", jobID, err)
		}
	default:
		return nil, fmt.Errorf("job %s pricing has unknown execution mode %q", jobID, pricing.ExecutionMode)
	}
	gotSHA256, err := pricingDecisionDigest(pricing)
	if err != nil {
		return nil, err
	}
	if frozenSHA256 == "" || frozenSHA256 != gotSHA256 {
		return nil, fmt.Errorf("frozen pricing decision digest mismatch for job %s", jobID)
	}
	return &pricing, nil
}

func (s *Store) JobWorkloadDecision(ctx context.Context, jobID uuid.UUID) (*WorkloadDecision, error) {
	inputs, err := s.loadJobComputeAuthorityInputs(ctx, jobID)
	if err != nil {
		return nil, err
	}
	return decodeJobWorkloadDecision(jobID, inputs.workloadBlob, inputs.workloadSHA256)
}

func (s *Store) JobComputePlan(ctx context.Context, jobID uuid.UUID) (*ComputePlan, error) {
	inputs, err := s.loadJobComputeAuthorityInputs(ctx, jobID)
	if err != nil {
		return nil, err
	}
	if len(inputs.computePlanBlob) == 0 {
		// Preserve the historical no-compute fast path: a legacy job remains
		// readable even when no execution geometry was frozen.
		return nil, nil
	}
	workload, err := decodeJobWorkloadDecision(
		jobID, inputs.workloadBlob, inputs.workloadSHA256,
	)
	if err != nil {
		return nil, err
	}
	return decodeJobComputePlan(
		jobID, inputs.computePlanBlob, inputs.computePlanSHA256,
		workload, inputs.economicPlanBlob,
	)
}

func (s *Store) loadJobReceiptComputeAuthority(
	ctx context.Context,
	jobID uuid.UUID,
) (*WorkloadDecision, *ComputePlan, error) {
	inputs, err := s.loadJobComputeAuthorityInputs(ctx, jobID)
	if err != nil {
		return nil, nil, err
	}
	workload, err := decodeJobWorkloadDecision(
		jobID, inputs.workloadBlob, inputs.workloadSHA256,
	)
	if err != nil {
		return nil, nil, err
	}
	plan, err := decodeJobComputePlan(
		jobID, inputs.computePlanBlob, inputs.computePlanSHA256,
		workload, inputs.economicPlanBlob,
	)
	if err != nil {
		return nil, nil, err
	}
	return workload, plan, nil
}

// The former per-document implementations are intentionally represented by the
// shared decoders above. All callers still receive the same validation and
// digest checks, but the immutable dependency row is fetched once.

func (s *Store) JobPlacementRequirement(ctx context.Context, jobID uuid.UUID) (*PlacementRequirement, error) {
	inputs, err := s.loadJobComputeAuthorityInputs(ctx, jobID)
	if err != nil {
		return nil, err
	}
	if len(inputs.placementBlob) == 0 {
		return nil, nil
	}
	workload, err := decodeJobWorkloadDecision(
		jobID, inputs.workloadBlob, inputs.workloadSHA256,
	)
	if err != nil {
		return nil, err
	}
	return decodeJobPlacementRequirement(
		jobID, inputs.placementBlob, inputs.placementSHA256, workload,
	)
}

func (s *Store) JobPricingDecision(ctx context.Context, jobID uuid.UUID) (*PricingDecision, error) {
	inputs, err := s.loadJobComputeAuthorityInputs(ctx, jobID)
	if err != nil {
		return nil, err
	}
	if len(inputs.pricingBlob) == 0 {
		return nil, nil
	}
	workload, err := decodeJobWorkloadDecision(
		jobID, inputs.workloadBlob, inputs.workloadSHA256,
	)
	if err != nil {
		return nil, err
	}
	compute, err := decodeJobComputePlan(
		jobID, inputs.computePlanBlob, inputs.computePlanSHA256,
		workload, inputs.economicPlanBlob,
	)
	if err != nil {
		return nil, err
	}
	return s.decodeJobPricingDecision(
		ctx, jobID, inputs.pricingBlob, inputs.pricingSHA256,
		inputs.placementBlob, inputs.placementSHA256,
		workload, compute, inputs.economicPlanBlob,
	)
}

type JobView struct {
	ID               uuid.UUID
	BuyerID          uuid.UUID
	Status           string
	JobType          string
	Tier             string
	OutputRef        string
	TaskCount        int
	TasksDone        int
	EstimatedUSD     float64
	ActualUSD        float64
	ETASecs          int
	CreatedAt        time.Time
	MaxUSD           float64
	BudgetState      string
	ChargeStatus     string
	Verification     Verification
	ResultsMergedAt  *time.Time
	ObjectsPurgedAt  *time.Time
	SLAGuaranteeSecs int
	SLAPremiumUSD    float64
	SLAMet           *bool
}

func (s *Store) JobSubmissionReplay(
	ctx context.Context,
	buyerID uuid.UUID,
	idempotencyKey, requestSHA256 string,
) (JobSubmitResponse, bool, error) {
	var out JobSubmitResponse
	var createdAt time.Time
	var tier, storedSHA, webhookID, webhookSecretSealed string
	err := s.pool.QueryRow(ctx, `
		SELECT j.id,COALESCE(j.task_count,0),COALESCE(j.estimated_usd,0)::float8,
		       COALESCE(j.eta_secs,0),COALESCE(j.tier,'batch'),j.created_at,
		       COALESCE(w.id::text,''),COALESCE(w.signing_secret_sealed,''),
		       COALESCE(j.submit_request_sha256,'')
		  FROM jobs j
		  LEFT JOIN LATERAL (
		    SELECT id,signing_secret_sealed FROM webhooks
		     WHERE job_id=j.id ORDER BY created_at,id LIMIT 1
		  ) w ON true
		 WHERE j.buyer_id=$1 AND j.submit_idempotency_key=$2`,
		buyerID, idempotencyKey,
	).Scan(&out.JobID, &out.TaskCount, &out.EstimatedUSD, &out.ETASecs, &tier,
		&createdAt, &webhookID, &webhookSecretSealed, &storedSHA)
	if errors.Is(err, pgx.ErrNoRows) {
		return JobSubmitResponse{}, false, nil
	}
	if err != nil {
		return JobSubmitResponse{}, false, err
	}
	if storedSHA != requestSHA256 {
		return JobSubmitResponse{}, true, errIdempotencyConflict
	}
	dur := time.Duration(out.ETASecs) * time.Second
	if min := tierMinCompletion(tier); dur < min {
		dur = min
	}
	out.EstimatedCompletion = createdAt.Add(dur).UTC().Format(time.RFC3339)
	out.TierSemantics = serviceTierSemantics(tier)
	out.WebhookID = webhookID
	if webhookSecretSealed != "" {
		secret, err := openWebhookSigningSecret(webhookSecretSealed)
		if err != nil {
			return JobSubmitResponse{}, true, fmt.Errorf("open replay webhook secret: %w", err)
		}
		out.WebhookSecret = secret
	}
	return out, true, nil
}

func (s *Store) GetJob(ctx context.Context, jobID, buyerID uuid.UUID) (*JobView, error) {
	var j JobView
	err := s.pool.QueryRow(ctx,
		`SELECT id, buyer_id, status, job_type, tier, COALESCE(output_ref,''),
		        COALESCE(task_count,0), COALESCE(tasks_done,0),
		        COALESCE(estimated_usd,0), COALESCE(actual_usd,0),
		        COALESCE(eta_secs,0), created_at,
		        COALESCE(max_usd,0), COALESCE(budget_state,'tracking'),
		        COALESCE(charge_status,'not_attempted'), results_merged_at, objects_purged_at,
		        COALESCE(sla_guarantee_secs,0), COALESCE(sla_premium_usd,0)::float8, sla_met
		 FROM jobs WHERE id = $1 AND buyer_id = $2`,
		jobID, buyerID,
	).Scan(&j.ID, &j.BuyerID, &j.Status, &j.JobType, &j.Tier, &j.OutputRef,
		&j.TaskCount, &j.TasksDone, &j.EstimatedUSD, &j.ActualUSD, &j.ETASecs, &j.CreatedAt,
		&j.MaxUSD, &j.BudgetState, &j.ChargeStatus, &j.ResultsMergedAt, &j.ObjectsPurgedAt,
		&j.SLAGuaranteeSecs, &j.SLAPremiumUSD, &j.SLAMet)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errNotFound
	}
	if err != nil {
		return nil, err
	}
	vr, verr := s.JobVerification(ctx, j.ID)
	if verr != nil {
		log.Printf("job verification aggregate (job %s): %v", j.ID, verr)
		vr.Label = deriveVerificationLabel(vr)
	}
	j.Verification = vr
	return &j, nil
}

type jobInternal struct {
	BuyerID            uuid.UUID
	TaskCount          int
	EstimatedUSD       float64
	Currency           string
	VerificationPolicy []byte
}

func (s *Store) getJobInternal(ctx context.Context, jobID uuid.UUID) (*jobInternal, error) {
	var j jobInternal
	err := s.pool.QueryRow(ctx,
		`SELECT buyer_id, COALESCE(task_count,0), COALESCE(estimated_usd,0),
		        currency, COALESCE(verification_policy,'{}'::jsonb)
		 FROM jobs WHERE id = $1`,
		jobID,
	).Scan(&j.BuyerID, &j.TaskCount, &j.EstimatedUSD, &j.Currency, &j.VerificationPolicy)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errNotFound
	}
	if err != nil {
		return nil, err
	}
	return &j, nil
}

func (s *Store) CancelJob(ctx context.Context, jobID, buyerID uuid.UUID) error {
	// Scope the very first lookup to the authenticated buyer. Besides avoiding
	// object-existence disclosure, this prevents one buyer from forcing locks on
	// another buyer's task rows.
	var status string
	err := s.pool.QueryRow(ctx,
		`SELECT status FROM jobs WHERE id=$1 AND buyer_id=$2`, jobID, buyerID).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return errNotFound
	}
	if err != nil {
		return err
	}
	if status == "cancelled" {
		return nil // DELETE is naturally idempotent for the owning buyer.
	}
	if status != "queued" {
		return errJobNotCancellable
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := lockUnfinishedJobTasksTx(ctx, tx, jobID); err != nil {
		return err
	}
	status, pending, err := jobTerminalTransitionStateTx(ctx, tx, jobID)
	if err != nil {
		return err
	}
	var owns bool
	if err := tx.QueryRow(ctx, `SELECT buyer_id=$2 FROM jobs WHERE id=$1`, jobID, buyerID).Scan(&owns); err != nil {
		return err
	}
	if !owns {
		return errNotFound
	}
	if status == "cancelled" {
		return tx.Commit(ctx)
	}
	if status != "queued" || pending {
		return errJobNotCancellable
	}
	if _, err := tx.Exec(ctx, `UPDATE jobs SET status='cancelled' WHERE id=$1`, jobID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx,
		`UPDATE tasks SET status = 'failed'
		 WHERE job_id = $1 AND status = 'queued'`, jobID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

const webhookRegistrationLimitPerJob = 32

func (s *Store) JobVerification(ctx context.Context, jobID uuid.UUID) (Verification, error) {
	var v Verification
	rows, err := s.pool.Query(ctx,
		`SELECT kind, count(*) FROM verification_events WHERE job_id = $1 GROUP BY kind`, jobID)
	if err != nil {
		return v, err
	}
	defer rows.Close()
	for rows.Next() {
		var kind string
		var n int
		if err := rows.Scan(&kind, &n); err != nil {
			return v, err
		}
		switch kind {
		case "honeypot_pass":
			v.HoneypotsPassed += n
			v.Checked += n
		case "honeypot_fail":
			v.HoneypotsFailed += n
			v.Checked += n
		case "redundancy_match":
			v.RedundancyMatched += n
			v.Checked += n
		case "redundancy_mismatch":
			v.RedundancyMismatched += n
			v.Checked += n
		case "tiebreak_win", "tiebreak_loss":
			v.Tiebreaks += n
			v.Checked += n
		case "redundancy_same_supplier":
			v.SameSupplier += n
		case "redundancy_cross_class", "tiebreak_cross_class":
			v.CrossClassSkipped += n
		}
	}
	if err := rows.Err(); err != nil {
		return v, err
	}
	if err := s.pool.QueryRow(ctx,
		`SELECT
		   (SELECT COUNT(DISTINCT COALESCE(t.chunk_index,0))
		      FROM tasks t
		     WHERE t.job_id=$1 AND t.status='complete'
		       AND t.is_honeypot=false AND t.is_redundancy=false),
		   (SELECT COUNT(DISTINCT COALESCE(et.chunk_index,0))
		      FROM verification_events ve
		      JOIN tasks et ON et.id=ve.task_id
		     WHERE ve.job_id=$1 AND et.job_id=$1
		       AND ve.kind IN ('redundancy_match','tiebreak_win'))`, jobID,
	).Scan(&v.DeliveredChunks, &v.VerifiedChunks); err != nil {
		return v, err
	}
	if v.VerifiedChunks > v.DeliveredChunks {
		v.VerifiedChunks = v.DeliveredChunks
	}
	v.UnverifiedChunks = v.DeliveredChunks - v.VerifiedChunks
	var disputeStatus string
	err = s.pool.QueryRow(ctx,
		`SELECT status FROM disputes WHERE job_id = $1 ORDER BY created_at DESC LIMIT 1`, jobID,
	).Scan(&disputeStatus)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return v, err
	}
	v.DisputeStatus = disputeStatus
	v.Label = deriveVerificationLabel(v)
	return v, nil
}

func (s *Store) JobVerificationClasses(ctx context.Context, jobID uuid.UUID) ([]string, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT DISTINCT COALESCE(vw.input_snapshot->>'engine',t.execution_engine,''),
		                 COALESCE(vw.input_snapshot->>'build_hash',t.execution_build_hash,''),
		                 COALESCE(vw.input_snapshot->>'build_identity_policy',t.execution_build_identity_policy,'')
		 FROM tasks t
		 LEFT JOIN verification_work vw ON vw.task_id=t.id AND vw.attempt=t.retry_count
		 WHERE t.job_id = $1 AND t.status = 'complete' AND t.is_honeypot = false`,
		jobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var engine, build, buildPolicy string
		if err := rows.Scan(&engine, &build, &buildPolicy); err != nil {
			return nil, err
		}
		out = append(out, classKey(engine, build, buildPolicy))
	}
	return out, rows.Err()
}

func (s *Store) JobCheckpointInfo(ctx context.Context, jobID uuid.UUID) (outputRef string, tasksDone int, err error) {
	err = s.pool.QueryRow(ctx,
		`SELECT COALESCE(output_ref,''), COALESCE(tasks_done,0) FROM jobs WHERE id = $1`,
		jobID).Scan(&outputRef, &tasksDone)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", 0, errNotFound
	}
	return outputRef, tasksDone, err
}

// DetachUnfinishedTasksForTerminalJob releases non-verification work that can
// no longer commit because its parent is durably terminal. It deliberately
// runs in its own transaction: callers must not add these sibling locks to a
// FailTaskTx transaction, whose established lock order is one task then job.
//
// Eligible tasks are locked in the same stable order as cancellation paths.
// The UPDATE is restricted to those exact IDs so a concurrent hedge/tiebreak
// insert is left for the durable janitor's next pass instead of being acquired
// out of order. Verification rows and execution provenance are evidence and
// remain untouched.
func (s *Store) DetachUnfinishedTasksForTerminalJob(
	ctx context.Context,
	jobID uuid.UUID,
) (int64, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)

	rows, err := tx.Query(ctx, `
		SELECT id
		  FROM tasks
		 WHERE job_id=$1 AND status IN ('queued','retrying','running')
		 ORDER BY id
		 FOR UPDATE`, jobID)
	if err != nil {
		return 0, err
	}
	var taskIDs []uuid.UUID
	for rows.Next() {
		var taskID uuid.UUID
		if err := rows.Scan(&taskID); err != nil {
			rows.Close()
			return 0, err
		}
		taskIDs = append(taskIDs, taskID)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if len(taskIDs) == 0 {
		return 0, tx.Commit(ctx)
	}

	var jobStatus string
	if err := tx.QueryRow(ctx, `SELECT status FROM jobs WHERE id=$1`, jobID).Scan(&jobStatus); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, tx.Commit(ctx)
		}
		return 0, err
	}
	var taskStatus string
	switch jobStatus {
	case "cancelled":
		taskStatus = "cancelled"
	case "failed", "complete":
		taskStatus = "failed"
	default:
		return 0, tx.Commit(ctx)
	}

	tag, err := tx.Exec(ctx, `
		UPDATE tasks
		   SET status=$3,
		       claimed_by=NULL,
		       claimed_at=NULL,
		       worker_id=NULL
		 WHERE job_id=$1
		   AND id=ANY($2::uuid[])
		   AND status IN ('queued','retrying','running')`,
		jobID, taskIDs, taskStatus)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// TerminalJobsWithUnfinishedTasks selects durable janitor work. Pending
// verification is intentionally not a filter here: once the parent is already
// terminal, non-verification tasks cannot commit and must not retain capacity.
// Verifying tasks and verification_work remain untouched by the detach helper.
func (s *Store) TerminalJobsWithUnfinishedTasks(
	ctx context.Context,
	limit int,
) ([]uuid.UUID, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT j.id
		  FROM jobs j
		 WHERE j.status IN ('failed','cancelled','complete')
		   AND EXISTS (
		     SELECT 1
		       FROM tasks t
		      WHERE t.job_id=j.id
		        AND t.status IN ('queued','retrying','running')
		   )
		 ORDER BY j.terminal_at NULLS FIRST, j.id
		 LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var jobIDs []uuid.UUID
	for rows.Next() {
		var jobID uuid.UUID
		if err := rows.Scan(&jobID); err != nil {
			return nil, err
		}
		jobIDs = append(jobIDs, jobID)
	}
	return jobIDs, rows.Err()
}

func failJobAndSettleOnce(ctx context.Context, tx pgx.Tx, jobID uuid.UUID) (flipped bool, err error) {
	status, pending, err := jobTerminalTransitionStateTx(ctx, tx, jobID)
	if err != nil {
		return false, err
	}
	if status == "complete" || status == "cancelled" || status == "failed" {
		return false, nil // already terminal  -  nothing to settle again
	}
	if pending {
		return false, ErrJobVerificationPending
	}
	ct, err := tx.Exec(ctx,
		`UPDATE jobs SET status = 'failed'
		  WHERE id = $1 AND status NOT IN ('complete','cancelled','failed')`, jobID)
	if err != nil {
		return false, err
	}
	if ct.RowsAffected() != 1 {
		return false, nil
	}
	if _, err := tx.Exec(ctx,
		`UPDATE jobs SET actual_usd = COALESCE((
		   SELECT SUM(-amount_usd) FROM ledger_entries
		   WHERE kind = 'buyer_charge'
		     AND task_id IN (SELECT id FROM tasks WHERE job_id = $1)
		 ),0)
		 WHERE id = $1`, jobID); err != nil {
		return false, err
	}
	return true, nil
}

type StuckJob struct {
	ID        uuid.UUID
	BuyerID   uuid.UUID
	OutputRef string
	EtaSecs   int
	TasksDone int
	TaskCount int
	Strikes   int  // watchdog_strikes: 0 -> rescue next, >=1 -> kill next
	DeadClaim bool // an unfinished task is claimed by a DEAD worker (machine-stuck, not workload-stuck)
}

func (s *Store) StuckRunningJobs(ctx context.Context, factor float64, grace, deadAfter time.Duration, limit int) ([]StuckJob, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT j.id, j.buyer_id, COALESCE(j.output_ref,''), COALESCE(j.eta_secs,0),
		        j.tasks_done, j.task_count, COALESCE(j.watchdog_strikes,0),
		        EXISTS (
		          SELECT 1 FROM tasks t JOIN workers w ON w.id = t.claimed_by
		          WHERE t.job_id = j.id AND t.status = 'running'
		            AND (w.last_seen_at IS NULL OR w.last_seen_at < now() - make_interval(secs => $4::float8))
		        ) AS dead_claim
		 FROM jobs j
		 WHERE j.status = 'running'
		   AND COALESCE(j.deadline_secs, 0) <> -1
		   AND (
		     (j.deadline_secs IS NOT NULL AND j.deadline_secs > 0
		       AND now() > j.created_at + make_interval(secs => j.deadline_secs::float8))
		     OR (COALESCE(j.deadline_secs, 0) = 0 AND COALESCE(j.eta_secs, 0) > 0
		       AND now() > j.created_at + GREATEST(
		             make_interval(secs => j.eta_secs::float8 * $1),
		             make_interval(secs => j.eta_secs::float8 + 120)))
		     OR (COALESCE(j.deadline_secs, 0) = 0 AND COALESCE(j.eta_secs, 0) = 0
		       AND now() > j.created_at + interval '24 hours')
		   )
		   AND NOT EXISTS (
		     SELECT 1 FROM tasks t
		     WHERE t.job_id = j.id
		       AND (t.completed_at > now() - make_interval(secs => $2::float8)
		         OR t.claimed_at   > now() - make_interval(secs => $2::float8)
		         OR t.visible_at   > now() - make_interval(secs => $2::float8))
		   )
		 ORDER BY j.created_at ASC
		 LIMIT $3`,
		factor, grace.Seconds(), limit, deadAfter.Seconds())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StuckJob
	for rows.Next() {
		var j StuckJob
		if err := rows.Scan(&j.ID, &j.BuyerID, &j.OutputRef, &j.EtaSecs, &j.TasksDone, &j.TaskCount,
			&j.Strikes, &j.DeadClaim); err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

func (s *Store) RescueStuckJob(ctx context.Context, jobID uuid.UUID, backoff time.Duration) (flipped bool, err error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	ct, err := tx.Exec(ctx,
		`UPDATE jobs SET watchdog_strikes = 1
		 WHERE id = $1 AND status = 'running' AND COALESCE(watchdog_strikes, 0) = 0`, jobID)
	if err != nil {
		return false, err
	}
	if ct.RowsAffected() == 0 {
		return false, nil
	}
	if _, err := tx.Exec(ctx,
		`UPDATE tasks
		   SET status = 'queued', claimed_by = NULL, claimed_at = NULL,
		       started_at = NULL, worker_id = NULL, retry_count = retry_count + 1,
		       visible_at = now() + make_interval(secs => $2)
		 WHERE job_id = $1 AND status IN ('running','retrying')`,
		jobID, backoff.Seconds()); err != nil {
		return false, err
	}
	if _, err := tx.Exec(ctx,
		`UPDATE tasks SET visible_at = now() + make_interval(secs => $2)
		 WHERE job_id = $1 AND status = 'queued'
		   AND visible_at < now() + make_interval(secs => $2)`,
		jobID, backoff.Seconds()); err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}

func (s *Store) CancelStuckJob(ctx context.Context, jobID uuid.UUID) (flipped bool, err error) {
	status, pending, err := s.jobTerminalTransitionState(ctx, jobID)
	if err != nil {
		return false, err
	}
	if status != "running" || pending {
		return false, nil
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	if err := lockUnfinishedJobTasksTx(ctx, tx, jobID); err != nil {
		return false, err
	}
	status, pending, err = jobTerminalTransitionStateTx(ctx, tx, jobID)
	if err != nil {
		return false, err
	}
	if status != "running" || pending {
		return false, nil
	}
	ct, err := tx.Exec(ctx,
		`UPDATE jobs SET status = 'cancelled' WHERE id = $1 AND status = 'running'`, jobID)
	if err != nil {
		return false, err
	}
	if ct.RowsAffected() == 0 {
		return false, nil
	}
	if _, err := tx.Exec(ctx,
		`UPDATE tasks SET status = 'cancelled'
		 WHERE job_id = $1 AND status NOT IN ('complete','failed','cancelled')`, jobID); err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}

type CompletableJob struct {
	ID        uuid.UUID
	BuyerID   uuid.UUID
	OutputRef string
}

var ErrJobNotFinalizable = errors.New("job is not finalizable")

func (s *Store) FinalizableJobs(ctx context.Context, limit int) ([]CompletableJob, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT j.id, j.buyer_id, COALESCE(j.output_ref,'')
		 FROM jobs j
		 WHERE j.status IN ('running','verifying')
		   AND j.task_count > 0
		   AND NOT EXISTS (
		     SELECT 1 FROM tasks t
		     WHERE t.job_id = j.id AND t.status NOT IN ('complete','failed')
		   )
		   AND EXISTS (SELECT 1 FROM tasks t WHERE t.job_id = j.id AND t.status = 'complete')
		 ORDER BY j.created_at ASC LIMIT $1`,
		limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CompletableJob
	for rows.Next() {
		var c CompletableJob
		if err := rows.Scan(&c.ID, &c.BuyerID, &c.OutputRef); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) MarkJobComplete(ctx context.Context, jobID uuid.UUID) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE jobs SET status = 'complete'
		 WHERE id = $1 AND status IN ('running','verifying')`,
		jobID)
	return err
}

func (s *Store) FinalizeJobTx(ctx context.Context, jobID uuid.UUID) error {
	return s.completeJobEconomics(ctx, jobID, nil)
}

func (s *Store) completeJobEconomics(
	ctx context.Context,
	jobID uuid.UUID,
	probe recoveryBoundaryProbe,
) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	var status string
	var buyerID uuid.UUID
	var prepaidRequired bool
	err = tx.QueryRow(ctx,
		`SELECT status FROM jobs WHERE id=$1 FOR UPDATE`, jobID,
	).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return errNotFound
	}
	if err != nil {
		return err
	}
	// The status lock above serializes terminalisation against task insertion.
	// Read frozen funding authority only after that lock is held.
	if err := tx.QueryRow(ctx,
		`SELECT buyer_id,prepaid_required FROM jobs WHERE id=$1`, jobID,
	).Scan(&buyerID, &prepaidRequired); err != nil {
		return err
	}
	if status != "complete" {
		if status != "running" && status != "verifying" {
			return fmt.Errorf("%w: job %s is %s", ErrJobNotFinalizable, jobID, status)
		}
		// Keep this as a separate statement after the job lock so READ COMMITTED
		// gives the finalizability check an explicit post-lock snapshot. Every
		// production task insertion takes this same job lock before its INSERT,
		// so either its committed obligation is visible here or it waits until
		// this transaction makes the parent terminal and then rolls back.
		var finalizable bool
		if err := tx.QueryRow(ctx, `
			SELECT j.task_count > 0
			       AND NOT EXISTS (
			         SELECT 1 FROM tasks t
			          WHERE t.job_id=j.id AND t.status NOT IN ('complete','failed')
			       )
			       AND EXISTS (
			         SELECT 1 FROM tasks t
			          WHERE t.job_id=j.id AND t.status='complete'
			       )
			  FROM jobs j WHERE j.id=$1`, jobID).Scan(&finalizable); err != nil {
			return err
		}
		if !finalizable {
			return fmt.Errorf("%w: job %s changed after finalizable selection",
				ErrJobNotFinalizable, jobID)
		}
		tag, err := tx.Exec(ctx,
			`UPDATE jobs SET status='complete'
			  WHERE id=$1 AND status IN ('running','verifying')`, jobID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return fmt.Errorf("%w: job %s lost live status", ErrJobNotFinalizable, jobID)
		}
	}
	reachRecoveryBoundary(ctx, probe, BoundaryCompleteAfterJobProjection)
	slaCharge, err := insertJobSLAPremiumChargeTx(ctx, tx, jobID, slaPremiumChargeRef(jobID))
	if err != nil {
		return err
	}
	if prepaidRequired && slaCharge.RowsAffected() == 1 {
		var slaPremiumMicros int64
		if err := tx.QueryRow(ctx, `
			SELECT (sla_premium_usd * 1000000)::bigint
			  FROM job_economic_plans WHERE job_id=$1`, jobID).Scan(&slaPremiumMicros); err != nil {
			return err
		}
		if err := debitPrepaidForSLAPremiumTx(ctx, tx, buyerID, jobID, slaPremiumMicros); err != nil {
			return fmt.Errorf("debit prepaid SLA premium for job %s: %w", jobID, err)
		}
	}
	reachRecoveryBoundary(ctx, probe, BoundaryCompleteAfterSLAPremium)
	if _, err := tx.Exec(ctx,
		`UPDATE jobs SET actual_usd = COALESCE((
		   SELECT SUM(-amount_usd) FROM ledger_entries
		   WHERE kind='buyer_charge'
		     AND (task_id IN (SELECT id FROM tasks WHERE job_id=$1)
		          OR payout_ref=$2)
		 ),0)
		 WHERE id=$1`, jobID, slaPremiumChargeRef(jobID)); err != nil {
		return err
	}
	// Accrue the modeled risk reserve when the frozen decision carries one. A
	// NULL pricing block is the explicit legacy no-op; a present but unreadable
	// modern decision is an authority failure and rolls this finalization back.
	pricing, pricingErr := s.JobPricingDecision(ctx, jobID)
	if pricingErr != nil {
		return fmt.Errorf("load frozen pricing authority for job %s: %w", jobID, pricingErr)
	}
	if pricing != nil {
		if err := AccrueRiskReserveAtSettlementTx(ctx, tx, jobID, *pricing); err != nil {
			return fmt.Errorf("accrue risk reserve for job %s: %w", jobID, err)
		}
	}
	reachRecoveryBoundary(ctx, probe, BoundaryCompleteAfterActualUSD)
	reachRecoveryBoundary(ctx, probe, BoundaryCompleteBeforeDBCommit)
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	reachRecoveryBoundary(ctx, probe, BoundaryCompleteAfterDBCommit)
	return nil
}

func (s *Store) SetJobActualUSD(ctx context.Context, jobID uuid.UUID) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE jobs SET actual_usd = COALESCE((
		   SELECT SUM(-amount_usd) FROM ledger_entries
		   WHERE kind = 'buyer_charge'
		     AND (task_id IN (SELECT id FROM tasks WHERE job_id = $1)
		          OR payout_ref = $2)
		 ),0)
		 WHERE id = $1`, jobID, slaPremiumChargeRef(jobID))
	return err
}

// JobPrimaryChunkCount returns how many primary ordinals a job was planned
// with. Media segment dispatch uses this as unit_count so every worker sees
// the same N without a separate plan table.
func (s *Store) JobPrimaryChunkCount(ctx context.Context, jobID uuid.UUID) (int, error) {
	plan, err := s.JobComputePlan(ctx, jobID)
	if err != nil {
		return 0, err
	}
	if plan == nil {
		return 0, errNotFound
	}
	if plan.PrimaryTasks > 0 {
		return plan.PrimaryTasks, nil
	}
	if plan.InputRecords > 0 {
		return plan.InputRecords, nil
	}
	var n int
	err = s.pool.QueryRow(ctx, `
		SELECT COUNT(DISTINCT COALESCE(chunk_index,0))
		  FROM tasks
		 WHERE job_id=$1 AND is_honeypot=false AND is_redundancy=false`, jobID).Scan(&n)
	return n, err
}

func (s *Store) JobResultKeys(ctx context.Context, jobID uuid.UUID) ([]string, error) {
	info, err := s.JobMergeInputs(ctx, jobID)
	if err != nil {
		return nil, err
	}
	out := make([]string, len(info.Results))
	for i := range info.Results {
		out[i] = info.Results[i].ResultRef
	}
	return out, nil
}

type JobMergeInfo struct {
	JobType   string
	OutputRef string
	Results   []PrimaryResult
}

func (s *Store) JobMergeInputs(ctx context.Context, jobID uuid.UUID) (*JobMergeInfo, error) {
	var info JobMergeInfo
	err := s.pool.QueryRow(ctx,
		`SELECT job_type, COALESCE(output_ref,'') FROM jobs WHERE id = $1`, jobID,
	).Scan(&info.JobType, &info.OutputRef)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errNotFound
	}
	if err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx,
		`WITH primary_results AS (
		   SELECT DISTINCT ON (COALESCE(chunk_index,0))
		          COALESCE(chunk_index,0) AS chunk_index,result_ref
		     FROM tasks
		    WHERE job_id=$1 AND status='complete'
		      AND is_honeypot=false AND is_redundancy=false
		      AND result_ref IS NOT NULL AND result_ref<>''
		    ORDER BY COALESCE(chunk_index,0),completed_at ASC NULLS LAST,id ASC
		 ), selected_resolution AS (
		   SELECT DISTINCT ON (chunk_index)
		          chunk_index,artifact_key,artifact_sha256,artifact_bytes
		     FROM chunk_artifact_resolutions
		    WHERE job_id=$1
		    ORDER BY chunk_index,
		             CASE basis WHEN 'majority' THEN 0 ELSE 1 END,
		             created_at DESC,effect_id DESC
		 )
		 SELECT p.chunk_index,COALESCE(r.artifact_key,p.result_ref),
		        r.artifact_key,r.artifact_sha256,r.artifact_bytes
		   FROM primary_results p
		   LEFT JOIN selected_resolution r USING (chunk_index)
		  ORDER BY p.chunk_index`,
		jobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var pr PrimaryResult
		var artifactKey, artifactSHA *string
		var artifactBytes *int64
		if err := rows.Scan(&pr.ChunkIndex, &pr.ResultRef, &artifactKey, &artifactSHA, &artifactBytes); err != nil {
			return nil, err
		}
		if artifactKey != nil || artifactSHA != nil || artifactBytes != nil {
			if artifactKey == nil || artifactSHA == nil || artifactBytes == nil {
				return nil, fmt.Errorf("chunk %d has an incomplete resolved artifact tuple", pr.ChunkIndex)
			}
			pr.Artifact = &VerificationArtifact{Key: *artifactKey, SHA256: *artifactSHA, Bytes: *artifactBytes}
		}
		info.Results = append(info.Results, pr)
	}
	return &info, rows.Err()
}
