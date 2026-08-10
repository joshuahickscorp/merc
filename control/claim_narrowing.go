package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ---------------------------------------------------------------------------
// Network V2 Step 18 — explicit narrowing ladder for batch claim
//
// Bible order: hard contract → trust/sandbox/privacy → region/failure domain →
// workload/runtime → artifact/model locality → prefix/cache locality →
// availability/queue → economic shortlist → expensive scoring.
//
// Production ClaimTasksTx already applies most hard filters inside one SQL
// statement (control/scheduler.go eligible_jobs / next). This package makes
// those stages first-class and measurable, and is honest about which Bible
// stages are only ORDER BY preferences or ABSENT as distinct filters.
//
// It does NOT invent a hierarchical candidate-index product, a frozen pull
// epoch, or a second admit authority. Reservation remains durable-row SQL
// under SKIP LOCKED / FOR UPDATE.
// ---------------------------------------------------------------------------

const (
	claimNarrowHardContract        = "hard_contract"
	claimNarrowTrustSandboxPrivacy = "trust_sandbox_privacy"
	claimNarrowRegionFailureDomain = "region_failure_domain"
	claimNarrowWorkloadRuntime     = "workload_runtime"
	claimNarrowArtifactLocality    = "artifact_model_locality"
	claimNarrowPrefixCacheLocality = "prefix_cache_locality"
	claimNarrowAvailabilityQueue   = "availability_queue"
	claimNarrowEconomicShortlist   = "economic_shortlist"
	claimNarrowExpensiveScoring    = "expensive_scoring"
)

const (
	// claimStageHardFilter drops candidates that fail the stage.
	claimStageHardFilter = "hard_filter"
	// claimStagePreferenceOnly is ORDER BY / soft signal only; count equals prior.
	claimStagePreferenceOnly = "preference_only"
	// claimStageSoftDeferral can hold a task briefly (cheaper_ask window).
	claimStageSoftDeferral = "soft_deferral"
	// claimStageAbsent is not a distinct production filter; count equals prior.
	claimStageAbsent = "absent"
)

// claimEligibilityTripwireRejectAll is production-false. Integration tests set
// it true to prove the eligibility-unchanged tripwire fails when a hard filter
// is deliberately broken (every job rejected via currency predicate).
var claimEligibilityTripwireRejectAll atomic.Bool

// claimNarrowingMeasureOnHotPath enables full Bible-stage measurement inside
// ClaimTasksTx. Default false: the measure re-evaluates fleet-relative EXISTS
// and must not double the residual batch cost on every poll. Tests and
// diagnostic probes set true. Production callers use MeasureClaimNarrowing
// explicitly when cardinalities are needed.
var claimNarrowingMeasureOnHotPath atomic.Bool

// ClaimNarrowingStageCount is one ladder rung with the job-candidate count
// that survived cumulative filters up to and including this stage.
type ClaimNarrowingStageCount struct {
	Stage     string `json:"stage"`
	Kind      string `json:"kind"`
	Surviving int    `json:"surviving"`
	Note      string `json:"note,omitempty"`
}

// ClaimObservationFamilies records the TTL families one batch claim SQL
// statement trusts. This is NOT a coherent network-epoch product and NOT a
// frozen candidate set (batch remains NONE_PULL_MARKET / SKIP LOCKED).
//
// A coherent epoch product that production and twin would share remains
// ABSENT until an epoch-backed shortlist proves parity with live SQL
// reservation (Step 18 shape note). Implementing that product here would
// invent a parallel authority.
type ClaimObservationFamilies struct {
	CandidateEpoch         string `json:"candidate_epoch"`
	WorkerPeerLivenessSecs int    `json:"worker_peer_liveness_secs"`
	WACAuthorizationDays   int    `json:"wac_authorization_days"`
	WarmModelSecs          int    `json:"warm_model_secs"`
	WarmPrefixSecs         int    `json:"warm_prefix_secs"`
	// RealtimeOfferSecs contrasts the parallel RT universe; batch does not bind it.
	RealtimeOfferSecs    int    `json:"realtime_offer_secs_not_bound"`
	StatementConsistency string `json:"statement_consistency"`
	// FailureDomainBound is false: failure_domain is ABSENT in control schema.
	FailureDomainBound bool   `json:"failure_domain_bound"`
	Note               string `json:"note,omitempty"`
}

// ClaimNarrowingTrace is the hot-path report for one ClaimTasksTx observation.
type ClaimNarrowingTrace struct {
	Stages                         []ClaimNarrowingStageCount `json:"stages"`
	Observation                    ClaimObservationFamilies   `json:"observation"`
	EligibleJobsAfterHardFilters   int                        `json:"eligible_jobs_after_hard_filters"`
	CheaperClassOnlineJobs         int                        `json:"cheaper_class_online_jobs"`
	CheaperAskOnlineJobs           int                        `json:"cheaper_ask_online_jobs"`
	ClaimableTasksAfterAskDeferral int                        `json:"claimable_tasks_after_ask_deferral"`
}

var claimNarrowingBibleOrder = []string{
	claimNarrowHardContract,
	claimNarrowTrustSandboxPrivacy,
	claimNarrowRegionFailureDomain,
	claimNarrowWorkloadRuntime,
	claimNarrowArtifactLocality,
	claimNarrowPrefixCacheLocality,
	claimNarrowAvailabilityQueue,
	claimNarrowEconomicShortlist,
	claimNarrowExpensiveScoring,
}

// DefaultClaimObservationFamilies returns the batch pull observation document.
func DefaultClaimObservationFamilies() ClaimObservationFamilies {
	return ClaimObservationFamilies{
		CandidateEpoch:         batchCandidateEpochNone,
		WorkerPeerLivenessSecs: 60,
		WACAuthorizationDays:   7,
		WarmModelSecs:          workerModelStateTTLSecs,
		WarmPrefixSecs:         workerPrefixStateTTLSecs,
		RealtimeOfferSecs:      realtimeOfferWarmthTTLSecs,
		StatementConsistency:   "READ_COMMITTED_SINGLE_STATEMENT",
		FailureDomainBound:     false,
		Note: "batch claim binds mixed TTL families inside one SQL statement " +
			"under READ COMMITTED; it does not publish a versioned coherent " +
			"network epoch shared with twin. Peer liveness 60s, wac 7d, " +
			"warm model 60s (preference), warm prefix 90s (preference). " +
			"Realtime 45s offer universe is a parallel lane and is not bound here. " +
			"failure_domain is ABSENT in control schema/selectors.",
	}
}

// ValidateClaimNarrowingLadder checks Bible order, kinds, and that surviving
// counts are non-increasing along the ladder (equivalently non-decreasing
// when read upward from expensive scoring toward hard contract).
func ValidateClaimNarrowingLadder(stages []ClaimNarrowingStageCount) error {
	if len(stages) != len(claimNarrowingBibleOrder) {
		return fmt.Errorf("claim narrowing ladder requires %d stages, got %d",
			len(claimNarrowingBibleOrder), len(stages))
	}
	for i, want := range claimNarrowingBibleOrder {
		if stages[i].Stage != want {
			return fmt.Errorf("claim narrowing stage %d: want %s got %s", i, want, stages[i].Stage)
		}
		if stages[i].Surviving < 0 {
			return fmt.Errorf("claim narrowing stage %s has negative surviving count", want)
		}
		switch stages[i].Kind {
		case claimStageHardFilter, claimStagePreferenceOnly, claimStageSoftDeferral, claimStageAbsent:
		default:
			return fmt.Errorf("claim narrowing stage %s has unknown kind %q", want, stages[i].Kind)
		}
		if i > 0 && stages[i].Surviving > stages[i-1].Surviving {
			return fmt.Errorf("claim narrowing stage %s surviving %d exceeds prior %s surviving %d (must be non-increasing down the ladder)",
				want, stages[i].Surviving, stages[i-1].Stage, stages[i-1].Surviving)
		}
		if stages[i].Kind == claimStagePreferenceOnly || stages[i].Kind == claimStageAbsent {
			if i == 0 {
				return fmt.Errorf("claim narrowing stage %s cannot be %s as the first stage", want, stages[i].Kind)
			}
			if stages[i].Surviving != stages[i-1].Surviving {
				return fmt.Errorf("claim narrowing stage %s is %s but surviving %d != prior %d",
					want, stages[i].Kind, stages[i].Surviving, stages[i-1].Surviving)
			}
			if strings.TrimSpace(stages[i].Note) == "" {
				return fmt.Errorf("claim narrowing stage %s (%s) requires a note", want, stages[i].Kind)
			}
		}
	}
	return nil
}

// claimCurrencyPredicate is the hard-contract currency filter. Production
// always requires jobs.currency = settlement ($7). The tripwire replaces it
// with FALSE so every job fails — proving eligibility tests catch a broken filter.
func claimCurrencyPredicate() string {
	if claimEligibilityTripwireRejectAll.Load() {
		// Keep $7 referenced so ClaimTasksTx always binds 7 args.
		return "($7::text IS NOT NULL AND FALSE) /* claimEligibilityTripwireRejectAll */"
	}
	return "j.currency = $7"
}

// meCTESQL is the claiming worker projection shared by measure queries.
const meCTESQL = `
me AS (
  SELECT w.id AS worker_id, w.supplier_id, w.hw_class,
         COALESCE(w.engine,'') AS engine, COALESCE(w.build_hash,'') AS build_hash,
         COALESCE(w.build_identity_policy,'') AS build_identity_policy,
         COALESCE(w.hardware_identity,'') AS hardware_identity,
         w.effective_memory_gb, w.memory_gb,
         w.min_payout_usd_hr, w.throttled,
         COALESCE(w.sandboxed,false) AS sandboxed,
         COALESCE(w.unsandboxed_opt_in,false) AS unsandboxed_opt_in,
         s.id AS supplier_id_s, s.status AS supplier_status,
         s.reputation, s.data_country
    FROM workers w
    JOIN suppliers s ON s.id = w.supplier_id
   WHERE w.id = $1
)`

// MeasureClaimNarrowing counts jobs surviving each Bible stage for one
// claiming worker, using the same durable predicates ClaimTasksTx applies.
// Preference-only and absent stages copy the prior count with an explicit note.
//
// Candidates are jobs (not workers): batch is pull — the worker is fixed and
// the queue is what narrows.
func (s *Store) MeasureClaimNarrowing(ctx context.Context, w WorkerAuth, selfCostRank int, selfMinPayoutUsdHr float64, settlementCurrency string) (ClaimNarrowingTrace, error) {
	if settlementCurrency == "" {
		return ClaimNarrowingTrace{}, errors.New("claim narrowing requires settlement currency")
	}
	if w.WorkerID == uuid.Nil {
		return ClaimNarrowingTrace{}, errors.New("claim narrowing requires worker_id")
	}

	var rep float32
	var jobsDone uint64
	if err := s.pool.QueryRow(ctx,
		`SELECT s.reputation, s.completed_tasks FROM suppliers s WHERE s.id = $1`,
		w.SupplierID,
	).Scan(&rep, &jobsDone); err != nil {
		return ClaimNarrowingTrace{}, err
	}
	tier := reputationTier(rep, jobsDone)

	// Cumulative stage flags for every non-terminal job. Matches eligible_jobs
	// hard filters in scheduler.go (currency via claimCurrencyPredicate).
	q := `
WITH params AS (
  SELECT $1::uuid AS worker_id,
         $2::int AS tier,
         $3::int AS self_cost_rank,
         $4::text AS matrix_sha,
         $5::float8 AS self_min_payout,
         $6::interval AS ask_window,
         $7::text AS settlement_currency
),
` + meCTESQL + `,
scored AS (
  SELECT
    j.id AS job_id,
    (
      j.status NOT IN ('cancelled','failed','complete')
      AND ` + claimCurrencyPredicate() + `
      AND COALESCE(j.min_memory_gb,0) <= COALESCE(me.effective_memory_gb, me.memory_gb, 0)
      AND (j.hw_classes IS NULL OR me.hw_class = ANY(j.hw_classes))
      AND COALESCE(j.offered_rate_usd_hr,1e9) >= COALESCE(me.min_payout_usd_hr,0)
      AND (
        COALESCE(j.placement_requirement->>'version','') IN ('','1','2')
        OR (
          j.placement_requirement->>'version' = '3'
          AND j.placement_requirement->>'engine_build_hash' = me.build_hash
          AND j.placement_requirement->>'engine_build_identity_policy' = me.build_identity_policy
          AND j.placement_requirement->>'hardware_identity' = me.hardware_identity
        )
      )
      AND (j.max_usd IS NULL OR (
            (SELECT COALESCE(SUM(-le.amount_usd),0) FROM ledger_entries le
              WHERE le.kind = 'buyer_charge'
                AND le.task_id IN (SELECT id FROM tasks WHERE job_id = j.id))
            + (SELECT COUNT(*) FROM tasks it
                  WHERE it.job_id = j.id AND it.status IN ('running','verifying'))
              * COALESCE((SELECT p.buyer_charge_per_task_usd FROM job_economic_plans p WHERE p.job_id=j.id),0)
            + COALESCE((SELECT p.buyer_charge_per_task_usd + p.sla_premium_usd FROM job_economic_plans p WHERE p.job_id=j.id),0)
          ) <= j.max_usd)
    ) AS pass_hard,
    (
      me.supplier_status = 'active'
      AND NOT COALESCE(me.throttled, false)
      AND COALESCE(j.min_reputation,0) <= me.reputation
      AND (j.tier <> 'trusted' OR $2 >= 2)
` + workerJobContainmentSQL("me", "j") + `
` + claimIndependenceSQL + `
    ) AS pass_trust,
    (j.data_residency IS NULL OR me.data_country = ANY(j.data_residency)) AS pass_region,
    EXISTS (
      SELECT 1 FROM worker_authorized_capabilities wac
       WHERE wac.worker_id = me.worker_id
         AND wac.job_type = j.job_type
         AND wac.model_ref = COALESCE(j.model_ref,'')
         AND wac.matrix_sha256 = $4
         AND wac.authorized_at >= now() - interval '7 days'
         AND wac.model_kind <> ''
         AND (
           (j.workload_decision IS NULL AND wac.routable)
           OR (
             EXISTS (
               SELECT 1
                 FROM jsonb_array_elements(
                   COALESCE(j.workload_decision->'runtime_candidates','[]'::jsonb)
                 ) frozen
                WHERE frozen->>'cell_id' = wac.cell_id
                  AND frozen->>'runtime_id' = wac.runtime_id
                  AND frozen->>'engine' = me.engine
                  AND wac.model_kind = COALESCE(
                        NULLIF(frozen->>'model_kind',''),
                        j.workload_decision #>> '{binding,model,kind}')
             )
           )
         )
    ) AS pass_runtime,
    EXISTS (
      SELECT 1 FROM tasks tt
       WHERE tt.job_id = j.id
         AND tt.status IN ('queued','retrying')
         AND tt.claimed_by IS NULL
         AND COALESCE(tt.visible_at, tt.created_at) <= now()
         AND (tt.excluded_worker IS NULL
              OR tt.excluded_worker <> $1
              OR tt.excluded_until IS NULL
              OR tt.excluded_until <= now())
    ) AS pass_ready,
    EXISTS (
      SELECT 1 FROM workers w2
        JOIN suppliers s2 ON s2.id = w2.supplier_id
      WHERE w2.id <> me.worker_id
        AND w2.last_seen_at IS NOT NULL
        AND w2.last_seen_at > now() - interval '60 seconds'
        AND s2.status = 'active'
        AND NOT COALESCE(w2.throttled, false)
` + workerJobContainmentSQL("w2", "j") + `
        AND (` + hwClassCostRankSQL("w2.hw_class") + `) < $3
        AND COALESCE(j.min_memory_gb,0) <= COALESCE(w2.effective_memory_gb, w2.memory_gb, 0)
        AND (j.hw_classes IS NULL OR w2.hw_class = ANY(j.hw_classes))
        AND (j.data_residency IS NULL OR s2.data_country = ANY(j.data_residency))
        AND COALESCE(j.min_reputation,0) <= COALESCE(s2.reputation,0)
        AND (
          j.tier <> 'trusted'
          OR (COALESCE(s2.reputation,0) >= 0.80 AND COALESCE(s2.completed_tasks,0) >= 500)
        )
        AND COALESCE(j.offered_rate_usd_hr,1e9) >= COALESCE(w2.min_payout_usd_hr,0)
        AND (
          COALESCE(j.placement_requirement->>'version','') IN ('','1','2')
          OR (
            j.placement_requirement->>'version' = '3'
            AND j.placement_requirement->>'engine_build_hash' = COALESCE(w2.build_hash,'')
            AND j.placement_requirement->>'engine_build_identity_policy' = COALESCE(w2.build_identity_policy,'')
            AND j.placement_requirement->>'hardware_identity' = COALESCE(w2.hardware_identity,'')
          )
        )
` + supplierNotLinkedToBuyerSQL("s2") + `
        AND EXISTS (
          SELECT 1 FROM worker_authorized_capabilities wac2
           WHERE wac2.worker_id = w2.id
             AND wac2.job_type = j.job_type
             AND wac2.model_ref = COALESCE(j.model_ref,'')
             AND wac2.matrix_sha256 = $4
             AND wac2.authorized_at >= now() - interval '7 days'
             AND (
               (j.workload_decision IS NULL AND wac2.routable)
               OR (
                 EXISTS (
                   SELECT 1
                     FROM jsonb_array_elements(
                       COALESCE(j.workload_decision->'runtime_candidates','[]'::jsonb)
                     ) frozen2
                    WHERE frozen2->>'cell_id' = wac2.cell_id
                      AND frozen2->>'runtime_id' = wac2.runtime_id
                      AND frozen2->>'engine' = COALESCE(w2.engine,'')
                      AND wac2.model_kind = COALESCE(
                            NULLIF(frozen2->>'model_kind',''),
                            j.workload_decision #>> '{binding,model,kind}')
                 )
               )
             )
        )
    ) AS cheaper_class_online,
    EXISTS (
      SELECT 1 FROM workers w3
        JOIN suppliers s3 ON s3.id = w3.supplier_id
      WHERE w3.id <> me.worker_id
        AND w3.last_seen_at IS NOT NULL
        AND w3.last_seen_at > now() - interval '60 seconds'
        AND s3.status = 'active'
        AND NOT COALESCE(w3.throttled, false)
` + workerJobContainmentSQL("w3", "j") + `
        AND COALESCE(w3.min_payout_usd_hr, 0) < $5
        AND COALESCE(j.offered_rate_usd_hr, 1e9) >= COALESCE(w3.min_payout_usd_hr, 0)
        AND COALESCE(j.min_memory_gb,0) <= COALESCE(w3.effective_memory_gb, w3.memory_gb, 0)
        AND (j.hw_classes IS NULL OR w3.hw_class = ANY(j.hw_classes))
        AND (j.data_residency IS NULL OR s3.data_country = ANY(j.data_residency))
        AND COALESCE(j.min_reputation,0) <= COALESCE(s3.reputation,0)
        AND (
          COALESCE(j.placement_requirement->>'version','') IN ('','1','2')
          OR (
            j.placement_requirement->>'version' = '3'
            AND j.placement_requirement->>'engine_build_hash' = COALESCE(w3.build_hash,'')
            AND j.placement_requirement->>'engine_build_identity_policy' = COALESCE(w3.build_identity_policy,'')
            AND j.placement_requirement->>'hardware_identity' = COALESCE(w3.hardware_identity,'')
          )
        )
        AND (
          j.tier <> 'trusted'
          OR (COALESCE(s3.reputation,0) >= 0.80 AND COALESCE(s3.completed_tasks,0) >= 500)
        )
` + supplierNotLinkedToBuyerSQL("s3") + `
        AND EXISTS (
          SELECT 1 FROM worker_authorized_capabilities wac3
           WHERE wac3.worker_id = w3.id
             AND wac3.job_type = j.job_type
             AND wac3.model_ref = COALESCE(j.model_ref,'')
             AND wac3.matrix_sha256 = $4
             AND wac3.authorized_at >= now() - interval '7 days'
             AND (
               (j.workload_decision IS NULL AND wac3.routable)
               OR (
                 EXISTS (
                   SELECT 1
                     FROM jsonb_array_elements(
                       COALESCE(j.workload_decision->'runtime_candidates','[]'::jsonb)
                     ) frozen3
                    WHERE frozen3->>'cell_id' = wac3.cell_id
                      AND frozen3->>'runtime_id' = wac3.runtime_id
                      AND frozen3->>'engine' = COALESCE(w3.engine,'')
                      AND wac3.model_kind = COALESCE(
                            NULLIF(frozen3->>'model_kind',''),
                            j.workload_decision #>> '{binding,model,kind}')
                 )
               )
             )
        )
    ) AS cheaper_ask_online,
    j.sla_guarantee_secs,
    j.eta_secs,
    j.created_at AS job_created_at
  FROM jobs j
  CROSS JOIN me
  WHERE j.status NOT IN ('cancelled','failed','complete')
)
SELECT
  COUNT(*) FILTER (WHERE pass_hard)::int,
  COUNT(*) FILTER (WHERE pass_hard AND pass_trust)::int,
  COUNT(*) FILTER (WHERE pass_hard AND pass_trust AND pass_region)::int,
  COUNT(*) FILTER (WHERE pass_hard AND pass_trust AND pass_region AND pass_runtime)::int,
  COUNT(*) FILTER (WHERE pass_hard AND pass_trust AND pass_region AND pass_runtime AND pass_ready)::int,
  COUNT(*) FILTER (WHERE pass_hard AND pass_trust AND pass_region AND pass_runtime AND pass_ready AND cheaper_class_online)::int,
  COUNT(*) FILTER (WHERE pass_hard AND pass_trust AND pass_region AND pass_runtime AND pass_ready AND cheaper_ask_online)::int
FROM scored`

	var hard, trust, region, runtime, ready int
	var cheaperClass, cheaperAsk int
	err := s.pool.QueryRow(ctx, q,
		w.WorkerID, int(tier), selfCostRank, generatedRuntimeMatrixSHA256,
		selfMinPayoutUsdHr, askDeferralWindow.String(), settlementCurrency,
	).Scan(&hard, &trust, &region, &runtime, &ready, &cheaperClass, &cheaperAsk)
	if err != nil {
		return ClaimNarrowingTrace{}, fmt.Errorf("measure claim narrowing: %w", err)
	}

	// Economic shortlist: jobs with ≥1 task that survives cheaper_ask hold.
	// Reuses the same soft-deferral predicate as the claim next CTE.
	econQ := `
WITH params AS (
  SELECT $1::uuid AS worker_id,
         $2::int AS tier,
         $3::int AS self_cost_rank,
         $4::text AS matrix_sha,
         $5::float8 AS self_min_payout,
         $6::interval AS ask_window,
         $7::text AS settlement_currency
),
` + meCTESQL + `,
eligible_jobs AS (
  SELECT j.id AS job_id, j.sla_guarantee_secs, j.eta_secs, j.created_at AS job_created_at,
    EXISTS (
      SELECT 1 FROM workers w3
        JOIN suppliers s3 ON s3.id = w3.supplier_id
      WHERE w3.id <> me.worker_id
        AND w3.last_seen_at IS NOT NULL
        AND w3.last_seen_at > now() - interval '60 seconds'
        AND s3.status = 'active'
        AND NOT COALESCE(w3.throttled, false)
` + workerJobContainmentSQL("w3", "j") + `
        AND COALESCE(w3.min_payout_usd_hr, 0) < $5
        AND COALESCE(j.offered_rate_usd_hr, 1e9) >= COALESCE(w3.min_payout_usd_hr, 0)
        AND COALESCE(j.min_memory_gb,0) <= COALESCE(w3.effective_memory_gb, w3.memory_gb, 0)
        AND (j.hw_classes IS NULL OR w3.hw_class = ANY(j.hw_classes))
        AND (j.data_residency IS NULL OR s3.data_country = ANY(j.data_residency))
        AND COALESCE(j.min_reputation,0) <= COALESCE(s3.reputation,0)
        AND (
          COALESCE(j.placement_requirement->>'version','') IN ('','1','2')
          OR (
            j.placement_requirement->>'version' = '3'
            AND j.placement_requirement->>'engine_build_hash' = COALESCE(w3.build_hash,'')
            AND j.placement_requirement->>'engine_build_identity_policy' = COALESCE(w3.build_identity_policy,'')
            AND j.placement_requirement->>'hardware_identity' = COALESCE(w3.hardware_identity,'')
          )
        )
        AND (
          j.tier <> 'trusted'
          OR (COALESCE(s3.reputation,0) >= 0.80 AND COALESCE(s3.completed_tasks,0) >= 500)
        )
` + supplierNotLinkedToBuyerSQL("s3") + `
        AND EXISTS (
          SELECT 1 FROM worker_authorized_capabilities wac3
           WHERE wac3.worker_id = w3.id
             AND wac3.job_type = j.job_type
             AND wac3.model_ref = COALESCE(j.model_ref,'')
             AND wac3.matrix_sha256 = $4
             AND wac3.authorized_at >= now() - interval '7 days'
             AND (
               (j.workload_decision IS NULL AND wac3.routable)
               OR (
                 EXISTS (
                   SELECT 1
                     FROM jsonb_array_elements(
                       COALESCE(j.workload_decision->'runtime_candidates','[]'::jsonb)
                     ) frozen3
                    WHERE frozen3->>'cell_id' = wac3.cell_id
                      AND frozen3->>'runtime_id' = wac3.runtime_id
                      AND frozen3->>'engine' = COALESCE(w3.engine,'')
                      AND wac3.model_kind = COALESCE(
                            NULLIF(frozen3->>'model_kind',''),
                            j.workload_decision #>> '{binding,model,kind}')
                 )
               )
             )
        )
    ) AS cheaper_ask_online
  FROM jobs j
  CROSS JOIN me
  WHERE j.status NOT IN ('cancelled','failed','complete')
    AND ` + claimCurrencyPredicate() + `
    AND COALESCE(j.min_memory_gb,0) <= COALESCE(me.effective_memory_gb, me.memory_gb, 0)
    AND NOT COALESCE(me.throttled, false)
    AND me.supplier_status = 'active'
    AND (j.hw_classes IS NULL OR me.hw_class = ANY(j.hw_classes))
    AND (j.data_residency IS NULL OR me.data_country = ANY(j.data_residency))
    AND COALESCE(j.min_reputation,0) <= me.reputation
    AND (j.tier <> 'trusted' OR $2 >= 2)
    AND COALESCE(j.offered_rate_usd_hr,1e9) >= COALESCE(me.min_payout_usd_hr,0)
    AND (
      COALESCE(j.placement_requirement->>'version','') IN ('','1','2')
      OR (
        j.placement_requirement->>'version' = '3'
        AND j.placement_requirement->>'engine_build_hash' = me.build_hash
        AND j.placement_requirement->>'engine_build_identity_policy' = me.build_identity_policy
        AND j.placement_requirement->>'hardware_identity' = me.hardware_identity
      )
    )
` + workerJobContainmentSQL("me", "j") + `
` + claimIndependenceSQL + `
    AND EXISTS (
      SELECT 1 FROM worker_authorized_capabilities wac
       WHERE wac.worker_id = me.worker_id
         AND wac.job_type = j.job_type
         AND wac.model_ref = COALESCE(j.model_ref,'')
         AND wac.matrix_sha256 = $4
         AND wac.authorized_at >= now() - interval '7 days'
         AND wac.model_kind <> ''
         AND (
           (j.workload_decision IS NULL AND wac.routable)
           OR (
             EXISTS (
               SELECT 1
                 FROM jsonb_array_elements(
                   COALESCE(j.workload_decision->'runtime_candidates','[]'::jsonb)
                 ) frozen
                WHERE frozen->>'cell_id' = wac.cell_id
                  AND frozen->>'runtime_id' = wac.runtime_id
                  AND frozen->>'engine' = me.engine
                  AND wac.model_kind = COALESCE(
                        NULLIF(frozen->>'model_kind',''),
                        j.workload_decision #>> '{binding,model,kind}')
             )
           )
         )
    )
)
SELECT
  COUNT(DISTINCT ej.job_id)::int,
  COUNT(*)::int
  FROM tasks t
  JOIN eligible_jobs ej ON ej.job_id = t.job_id
 WHERE t.status IN ('queued','retrying')
   AND t.claimed_by IS NULL
   AND COALESCE(t.visible_at, t.created_at) <= now()
   AND (t.excluded_worker IS NULL
        OR t.excluded_worker <> $1
        OR t.excluded_until IS NULL
        OR t.excluded_until <= now())
   AND (NOT ej.cheaper_ask_online
        OR COALESCE(t.visible_at, t.created_at) <= now() - $6::interval
        OR (
          COALESCE(ej.sla_guarantee_secs, 0) > 0
          AND (
            ej.eta_secs IS NULL
            OR (
              EXTRACT(EPOCH FROM (now() - ej.job_created_at))
              + EXTRACT(EPOCH FROM ($6::interval))
              + ej.eta_secs
            ) > ej.sla_guarantee_secs
          )
        ))`

	var economicJobs, claimableTasks int
	if err := s.pool.QueryRow(ctx, econQ,
		w.WorkerID, int(tier), selfCostRank, generatedRuntimeMatrixSHA256,
		selfMinPayoutUsdHr, askDeferralWindow.String(), settlementCurrency,
	).Scan(&economicJobs, &claimableTasks); err != nil {
		return ClaimNarrowingTrace{}, fmt.Errorf("measure economic shortlist: %w", err)
	}
	if economicJobs > ready {
		economicJobs = ready
	}

	stages := []ClaimNarrowingStageCount{
		{Stage: claimNarrowHardContract, Kind: claimStageHardFilter, Surviving: hard,
			Note: "currency, memory, hw_classes, offered_rate floor, placement v3, budget governor, non-terminal status"},
		{Stage: claimNarrowTrustSandboxPrivacy, Kind: claimStageHardFilter, Surviving: trust,
			Note: "supplier active, reputation, trusted tier, containment, buyer-supplier independence, not throttled"},
		{Stage: claimNarrowRegionFailureDomain, Kind: claimStageHardFilter, Surviving: region,
			Note: "data_residency only; failure_domain ABSENT in control schema/selectors"},
		{Stage: claimNarrowWorkloadRuntime, Kind: claimStageHardFilter, Surviving: runtime,
			Note: "worker_authorized_capabilities exact cell/runtime/matrix within 7d"},
		{Stage: claimNarrowArtifactLocality, Kind: claimStagePreferenceOnly, Surviving: runtime,
			Note: "warm_for_task is ORDER BY preference only (worker_model_state 60s); not a hard filter"},
		{Stage: claimNarrowPrefixCacheLocality, Kind: claimStagePreferenceOnly, Surviving: runtime,
			Note: "warm_prefix_depth is ORDER BY preference only (worker_prefix_state 90s); not a hard filter"},
		{Stage: claimNarrowAvailabilityQueue, Kind: claimStageHardFilter, Surviving: ready,
			Note: "ready unclaimed/retrying visible task; excluded_worker window; SKIP LOCKED at reservation"},
		{Stage: claimNarrowEconomicShortlist, Kind: claimStageSoftDeferral, Surviving: economicJobs,
			Note: "cheaper_ask_online holds tasks for askDeferralWindow when slack allows; cheaper_class_online is ORDER BY only"},
		{Stage: claimNarrowExpensiveScoring, Kind: claimStageAbsent, Surviving: economicJobs,
			Note: "no first-class expensive scorer stage; preferences remain ORDER BY terms inside claim SQL"},
	}
	if err := ValidateClaimNarrowingLadder(stages); err != nil {
		return ClaimNarrowingTrace{}, err
	}

	return ClaimNarrowingTrace{
		Stages:                         stages,
		Observation:                    DefaultClaimObservationFamilies(),
		EligibleJobsAfterHardFilters:   ready,
		CheaperClassOnlineJobs:         cheaperClass,
		CheaperAskOnlineJobs:           cheaperAsk,
		ClaimableTasksAfterAskDeferral: claimableTasks,
	}, nil
}

// listEligibleClaimJobIDs returns job IDs that pass hard filters through
// availability for this worker. Used by the eligibility-unchanged tripwire.
func (s *Store) listEligibleClaimJobIDs(ctx context.Context, w WorkerAuth, selfCostRank int, selfMinPayoutUsdHr float64, settlementCurrency string) ([]uuid.UUID, error) {
	var rep float32
	var jobsDone uint64
	if err := s.pool.QueryRow(ctx,
		`SELECT s.reputation, s.completed_tasks FROM suppliers s WHERE s.id = $1`,
		w.SupplierID,
	).Scan(&rep, &jobsDone); err != nil {
		return nil, err
	}
	tier := reputationTier(rep, jobsDone)
	q := `
WITH params AS (
  SELECT $1::uuid AS worker_id,
         $2::int AS tier,
         $3::int AS self_cost_rank,
         $4::text AS matrix_sha,
         $5::float8 AS self_min_payout,
         $6::interval AS ask_window,
         $7::text AS settlement_currency
),
` + meCTESQL + `
SELECT j.id
  FROM jobs j
  CROSS JOIN me
 WHERE j.status NOT IN ('cancelled','failed','complete')
   AND ` + claimCurrencyPredicate() + `
   AND COALESCE(j.min_memory_gb,0) <= COALESCE(me.effective_memory_gb, me.memory_gb, 0)
   AND NOT COALESCE(me.throttled, false)
   AND me.supplier_status = 'active'
   AND (j.hw_classes IS NULL OR me.hw_class = ANY(j.hw_classes))
   AND (j.data_residency IS NULL OR me.data_country = ANY(j.data_residency))
   AND COALESCE(j.min_reputation,0) <= me.reputation
   AND (j.tier <> 'trusted' OR $2 >= 2)
   AND COALESCE(j.offered_rate_usd_hr,1e9) >= COALESCE(me.min_payout_usd_hr,0)
   AND (
     COALESCE(j.placement_requirement->>'version','') IN ('','1','2')
     OR (
       j.placement_requirement->>'version' = '3'
       AND j.placement_requirement->>'engine_build_hash' = me.build_hash
       AND j.placement_requirement->>'engine_build_identity_policy' = me.build_identity_policy
       AND j.placement_requirement->>'hardware_identity' = me.hardware_identity
     )
   )
` + workerJobContainmentSQL("me", "j") + `
` + claimIndependenceSQL + `
   AND EXISTS (
     SELECT 1 FROM worker_authorized_capabilities wac
      WHERE wac.worker_id = me.worker_id
        AND wac.job_type = j.job_type
        AND wac.model_ref = COALESCE(j.model_ref,'')
        AND wac.matrix_sha256 = $4
        AND wac.authorized_at >= now() - interval '7 days'
        AND wac.model_kind <> ''
        AND (
          (j.workload_decision IS NULL AND wac.routable)
          OR (
            EXISTS (
              SELECT 1
                FROM jsonb_array_elements(
                  COALESCE(j.workload_decision->'runtime_candidates','[]'::jsonb)
                ) frozen
               WHERE frozen->>'cell_id' = wac.cell_id
                 AND frozen->>'runtime_id' = wac.runtime_id
                 AND frozen->>'engine' = me.engine
                 AND wac.model_kind = COALESCE(
                       NULLIF(frozen->>'model_kind',''),
                       j.workload_decision #>> '{binding,model,kind}')
            )
          )
         )
   )
   AND EXISTS (
     SELECT 1 FROM tasks tt
      WHERE tt.job_id = j.id
        AND tt.status IN ('queued','retrying')
        AND tt.claimed_by IS NULL
        AND COALESCE(tt.visible_at, tt.created_at) <= now()
   )
 ORDER BY j.id`
	// $3/$5/$6 unused but keep parameter positions aligned with claim SQL.
	_ = selfCostRank
	_ = selfMinPayoutUsdHr
	rows, err := s.pool.Query(ctx, q,
		w.WorkerID, int(tier), 0, generatedRuntimeMatrixSHA256,
		0.0, askDeferralWindow.String(), settlementCurrency,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// explainClaimExistsSubplan returns EXPLAIN (ANALYZE, BUFFERS, FORMAT TEXT)
// for a cheaper_ask_online-shaped EXISTS over live workers for one job.
func explainClaimExistsSubplan(ctx context.Context, pool *pgxpool.Pool, jobID, claimWorkerID uuid.UUID, selfMinPayout float64, matrixSHA string) (string, error) {
	q := `
EXPLAIN (ANALYZE, BUFFERS, FORMAT TEXT)
SELECT EXISTS (
  SELECT 1 FROM workers w3
    JOIN suppliers s3 ON s3.id = w3.supplier_id
  WHERE w3.id <> $1
    AND w3.last_seen_at IS NOT NULL
    AND w3.last_seen_at > now() - interval '60 seconds'
    AND s3.status = 'active'
    AND NOT COALESCE(w3.throttled, false)
    AND COALESCE(w3.min_payout_usd_hr, 0) < $3
    AND EXISTS (
      SELECT 1 FROM worker_authorized_capabilities wac3
       WHERE wac3.worker_id = w3.id
         AND wac3.job_type = (SELECT job_type FROM jobs WHERE id = $2)
         AND wac3.model_ref = COALESCE((SELECT model_ref FROM jobs WHERE id = $2),'')
         AND wac3.matrix_sha256 = $4
         AND wac3.authorized_at >= now() - interval '7 days'
    )
)`
	rows, err := pool.Query(ctx, q, claimWorkerID, jobID, selfMinPayout, matrixSHA)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var b strings.Builder
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			return "", err
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String(), rows.Err()
}

// createClaimPeerIndexes applies supporting indexes for cheaper_* peer scans.
// Idempotent; does not change claim predicates or eligibility.
func createClaimPeerIndexes(ctx context.Context, pool *pgxpool.Pool) error {
	stmts := []string{
		`CREATE INDEX IF NOT EXISTS workers_live_ask_seen_idx
		   ON workers (min_payout_usd_hr ASC, last_seen_at DESC, id)
		   WHERE last_seen_at IS NOT NULL AND NOT COALESCE(throttled, false)`,
		`CREATE INDEX IF NOT EXISTS workers_live_hwclass_seen_idx
		   ON workers (hw_class, last_seen_at DESC, id)
		   WHERE last_seen_at IS NOT NULL AND NOT COALESCE(throttled, false)`,
		`CREATE INDEX IF NOT EXISTS worker_authorized_capabilities_fresh_supply_idx
		   ON worker_authorized_capabilities (job_type, model_ref, matrix_sha256, authorized_at DESC, worker_id)`,
	}
	for _, stmt := range stmts {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("create claim peer index: %w", err)
		}
	}
	return nil
}

// RealtimeAuthorizeNarrowingStages documents RT authorize stage cardinalities.
// Intermediate Bible stages are not separately counted in production offer SQL;
// the sole published count is the final eligible book size (profile-scoped).
func RealtimeAuthorizeNarrowingStages(finalCandidateCount int) []ClaimNarrowingStageCount {
	n := finalCandidateCount
	if n < 0 {
		n = 0
	}
	stages := []ClaimNarrowingStageCount{
		{Stage: claimNarrowHardContract, Kind: claimStageHardFilter, Surviving: n,
			Note: "profile+sha, ACTIVE, capacity, rate ceilings collapsed with other RT hard filters into one book CTE"},
		{Stage: claimNarrowTrustSandboxPrivacy, Kind: claimStageHardFilter, Surviving: n,
			Note: "supplier active + not quarantined applied in same candidates CTE; not a separate cardinality"},
		{Stage: claimNarrowRegionFailureDomain, Kind: claimStageAbsent, Surviving: n,
			Note: "realtime offer SQL has no region filter; failure_domain ABSENT"},
		{Stage: claimNarrowWorkloadRuntime, Kind: claimStageHardFilter, Surviving: n,
			Note: "runtime_profile_id + runtime_profile_sha256 scope the offer book (not full fleet)"},
		{Stage: claimNarrowArtifactLocality, Kind: claimStagePreferenceOnly, Surviving: n,
			Note: "warmth is ORDER BY after verified-outcome cost"},
		{Stage: claimNarrowPrefixCacheLocality, Kind: claimStageAbsent, Surviving: n,
			Note: "prefix cache not a realtime authorize filter"},
		{Stage: claimNarrowAvailabilityQueue, Kind: claimStageHardFilter, Surviving: n,
			Note: "available_sequences > 0 and last_seen_at within 45s in same CTE"},
		{Stage: claimNarrowEconomicShortlist, Kind: claimStageHardFilter, Surviving: n,
			Note: "verified-outcome cost rank + SKIP LOCKED / FOR UPDATE reservation; final candidate_count is this book size"},
		{Stage: claimNarrowExpensiveScoring, Kind: claimStageAbsent, Surviving: n,
			Note: "no separate expensive scorer beyond verified-outcome cost ORDER BY"},
	}
	_ = ValidateClaimNarrowingLadder(stages)
	return stages
}

// Keep time referenced for ClaimedAt / observation docs.
var _ = time.RFC3339
