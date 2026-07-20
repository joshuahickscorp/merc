package main

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

var ErrNoSupply = errors.New("no eligible supply")

var noHedgePeerFound atomic.Int64

func NoHedgePeerCount() int64 { return noHedgePeerFound.Load() }

var claimDurationBucketsMs = []float64{1, 2, 5, 10, 25, 50, 100, 250, 500, 1000, 2500, 5000}

type claimHistogram struct {
	buckets  [12]atomic.Int64 // cumulative count, observation <= bucket bound (len == len(claimDurationBucketsMs))
	overflow atomic.Int64     // observation > every bound (folded into +Inf)
	sumMs    atomic.Int64     // sum of all observations, ms (for the _sum series)
	count    atomic.Int64     // total observations (for the _count series)
}

var claimDuration claimHistogram

func init() {
	if len(claimDurationBucketsMs) != len(claimDuration.buckets) {
		panic("claimDurationBucketsMs length must match claimHistogram.buckets length")
	}
}

func (h *claimHistogram) observe(d time.Duration) {
	ms := float64(d) / float64(time.Millisecond)
	h.count.Add(1)
	h.sumMs.Add(int64(ms))
	placed := false
	for i, bound := range claimDurationBucketsMs {
		if ms <= bound {
			h.buckets[i].Add(1)
			placed = true
			break
		}
	}
	if !placed {
		h.overflow.Add(1)
	}
}

func (h *claimHistogram) snapshot() (cumulative []int64, count int64, sumMs int64) {
	cumulative = make([]int64, len(claimDurationBucketsMs))
	var running int64
	for i := range claimDurationBucketsMs {
		running += h.buckets[i].Load()
		cumulative[i] = running
	}
	return cumulative, h.count.Load(), h.sumMs.Load()
}

type MatchTask struct {
	JobType      string
	MinMemoryGB  float32
	HWClasses    []string // nil/empty = any
	Tier         string   // batch | priority | trusted
	PinEngine    string
	PinBuildHash string
}

type MatchWorker struct {
	ID              uuid.UUID
	SupplierID      uuid.UUID
	HWClass         string
	Engine          string
	BuildHash       string
	MemoryGB        float32
	Reputation      float32
	TPS             map[string]float32 // job_type -> tokens/sec
	LastSeen        time.Time
	Tier            int  // 0-3
	Throttled       bool // currently pausing new claims (memory pressure)
	Warm            bool // already has the job's model warm in its pool (re-rank bonus only)
	ThermalDegraded bool
}

func Match(t MatchTask, workers []MatchWorker) ([]MatchWorker, error) {
	var candidates []MatchWorker
	for _, w := range workers {
		if time.Since(w.LastSeen) > 60*time.Second {
			continue // stale liveness
		}
		if w.Throttled {
			continue // pausing for memory pressure  -  unsafe to dispatch
		}
		if w.MemoryGB < t.MinMemoryGB {
			continue
		}
		if len(t.HWClasses) > 0 && !containsStr(t.HWClasses, w.HWClass) {
			continue
		}
		if t.PinEngine != "" && w.Engine != t.PinEngine {
			continue
		}
		if t.PinBuildHash != "" && w.BuildHash != t.PinBuildHash {
			continue
		}
		if t.Tier == "trusted" && w.Tier < 2 {
			continue
		}
		candidates = append(candidates, w)
	}
	if len(candidates) == 0 {
		return nil, ErrNoSupply
	}
	sort.Slice(candidates, func(i, j int) bool {
		return matchScore(candidates[i], t.JobType) > matchScore(candidates[j], t.JobType)
	})
	winClass := candidates[0].HWClass
	same := candidates[:0:0]
	for _, w := range candidates {
		if w.HWClass == winClass {
			same = append(same, w)
		}
	}
	return same, nil
}

const warmBonus = 1.05

const thermalPenalty = 0.7

func matchScore(w MatchWorker, jobType string) float32 {
	s := w.Reputation * w.TPS[jobType]
	if w.Warm {
		s *= warmBonus
	}
	if w.ThermalDegraded {
		s *= thermalPenalty
	}
	return s
}

func containsStr(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

func hwClassCostRank(hwClass string) int {
	switch hwClass {
	case "apple_silicon_base":
		return 0
	case "apple_silicon_pro":
		return 1
	case "apple_silicon_max":
		return 2
	case "apple_silicon_ultra":
		return 3
	default:
		return 0 // unknown/new class: treat as cheapest so it is never deprioritized
	}
}

func hwClassCostRankSQL(col string) string {
	return `CASE ` + col + `
	          WHEN 'apple_silicon_base' THEN 0
	          WHEN 'apple_silicon_pro' THEN 1
	          WHEN 'apple_silicon_max' THEN 2
	          WHEN 'apple_silicon_ultra' THEN 3
	          ELSE 0
	        END`
}

func (s *Store) SelectRedundancyPeer(ctx context.Context, jobType, modelRef string, minMemGB float32, primaryWorker uuid.UUID) (uuid.UUID, error) {
	return s.SelectRedundancyPeerExcluding(ctx, jobType, modelRef, minMemGB, primaryWorker, nil, nil)
}

func (s *Store) SelectRedundancyPeerExcluding(ctx context.Context, jobType, modelRef string, minMemGB float32, anchor uuid.UUID, also, alsoSuppliers []uuid.UUID) (uuid.UUID, error) {
	primary, err := s.GetWorkerProfile(ctx, anchor)
	if err != nil {
		return uuid.Nil, err
	}

	candidates, err := s.CandidateWorkers(ctx, jobType, modelRef, minMemGB)
	if err != nil {
		return uuid.Nil, err
	}
	pruned := prunePeers(candidates, anchor, primary.SupplierID, also, alsoSuppliers)

	ranked, err := Match(MatchTask{
		JobType:      jobType,
		MinMemoryGB:  minMemGB,
		HWClasses:    []string{primary.HWClass}, // same hw class only
		PinEngine:    primary.Engine,
		PinBuildHash: primary.BuildHash,
		Tier:         "batch",
	}, pruned)
	if err != nil {
		if errors.Is(err, ErrNoSupply) && len(pruned) > 0 {
			noHedgePeerFound.Add(1)
		}
		return uuid.Nil, err // ErrNoSupply when no same-class peer is free
	}
	return rankPeersBySpeed(ranked, jobType)[0].ID, nil
}

func (s *Store) SelectEndgameRacePeer(ctx context.Context, jobType, modelRef string, minMemGB float32, anchor uuid.UUID) (uuid.UUID, error) {
	primary, err := s.GetWorkerProfile(ctx, anchor)
	if err != nil {
		return uuid.Nil, err
	}
	candidates, err := s.CandidateWorkers(ctx, jobType, modelRef, minMemGB)
	if err != nil {
		return uuid.Nil, err
	}
	pruned := prunePeers(candidates, anchor, primary.SupplierID, nil, nil)
	ranked, err := Match(MatchTask{
		JobType:      jobType,
		MinMemoryGB:  minMemGB,
		HWClasses:    []string{primary.HWClass},
		PinEngine:    primary.Engine,
		PinBuildHash: primary.BuildHash,
		Tier:         "batch",
	}, pruned)
	if err != nil {
		return uuid.Nil, err // ErrNoSupply when no same-class peer is online at all
	}
	ids := make([]uuid.UUID, len(ranked))
	for i, w := range ranked {
		ids[i] = w.ID
	}
	busy, err := s.BusyWorkerIDs(ctx, ids)
	if err != nil {
		return uuid.Nil, err
	}
	idle := make([]MatchWorker, 0, len(ranked))
	for _, w := range ranked {
		if busy[w.ID] {
			continue
		}
		if modelRef != "" && !w.Warm {
			continue // never cold-race: a minutes-long load cannot cut a seconds tail
		}
		idle = append(idle, w)
	}
	if len(idle) == 0 {
		return uuid.Nil, ErrNoSupply
	}
	return rankPeersBySpeed(idle, jobType)[0].ID, nil
}

func prunePeers(candidates []MatchWorker, anchor, anchorSupplier uuid.UUID, also, alsoSuppliers []uuid.UUID) []MatchWorker {
	excluded := map[uuid.UUID]bool{anchor: true}
	for _, id := range also {
		excluded[id] = true
	}
	excludedSup := map[uuid.UUID]bool{}
	if anchorSupplier != uuid.Nil {
		excludedSup[anchorSupplier] = true
	}
	for _, id := range alsoSuppliers {
		if id != uuid.Nil {
			excludedSup[id] = true
		}
	}
	out := make([]MatchWorker, 0, len(candidates))
	for _, c := range candidates {
		if excluded[c.ID] {
			continue
		}
		if excludedSup[c.SupplierID] {
			continue // anchor's (or another disputant's) supplier  -  not independent
		}
		out = append(out, c)
	}
	return out
}

type ClaimedTask struct {
	TaskID           uuid.UUID
	Attempt          int16
	JobID            uuid.UUID
	JobType          string
	ModelRef         string
	ModelKind        string
	RuntimeCellID    string
	RuntimeID        string
	RuntimeMatrixSHA string
	InputRef         string // this task's input chunk key (presigned to input_url)
	ResultKey        string // where the worker writes its result (presigned to output_url)
	OutputRef        string // the job-level merged output key (manifest)
	Tier             string
	MinMemoryGB      float32
	HWClasses        []string
	MaxDurationSecs  uint32
	DataResidency    []string
	VerifPolicy      []byte
	JobTypeSpec      []byte // full submitted JobType JSON (tag + labels/schema/max_tokens/...)
	OfferedRateUsdHr float32
	ChunkIndex       int
	IsHoneypot       bool
}

func ClaimTaskSQL(claimedByPredicate string) string {
	return fmt.Sprintf(`WITH me AS (
	   -- The ONE claiming worker + its supplier, resolved ONCE (w.id = $1). Every
	   -- per-JOB hard filter below (memory, hw_classes, exact runtime capability,
	   -- residency, reputation, tier, and payout floor) compares the
	   -- JOB against THIS worker/supplier  -  none of it depends on the individual
	   -- task, so it belongs here, computed once per job, not once per task.
	   SELECT w.id AS worker_id, w.supplier_id, w.hw_class,
	          COALESCE(w.engine,'') AS engine, COALESCE(w.build_hash,'') AS build_hash,
	          w.effective_memory_gb, w.memory_gb,
		          w.min_payout_usd_hr, w.throttled,
		          COALESCE(w.priority_claim_streak,0) AS priority_claim_streak,
		          s.id AS supplier_id_s, s.status AS supplier_status,
	          s.reputation, s.data_country
	     FROM workers w
	     JOIN suppliers s ON s.id = w.supplier_id
	    WHERE w.id = $1
	 ),
	 -- PATCH (Control plane hot path 8->9, docs/internal/CREED_AND_PATH_TO_TEN.md
	 -- Get the correlated-subquery cost out of the transactional hot path.
	 -- the O(queue x fleet) cost entry 61 root-caused). eligible_jobs enforces
	 -- EVERY per-job-only predicate (the whole hard filter EXCEPT the two genuinely
	 -- per-task conditions: t.status/visible_at and the claimed_by branch) and
	 -- computes cheaper_class_online ONCE PER CANDIDATE JOB. It is MATERIALIZED on
	 -- purpose: without the barrier Postgres inlines this CTE back into the
	 -- tasks->jobs fan-out and re-evaluates the cheaper_class_online workers-scan
	 -- once per TASK (measured at a 12k-task/600-worker backlog: SubPlan seq-scanning
	 -- workers 12,001 times), which is exactly the pre-existing cost entry 61
	 -- reported. With MATERIALIZED it runs once per candidate JOB (~240 vs 12,000
	 -- at that scale), a >10x cut in the fleet scans. cheaper_class_online depends
	 -- only on j.* and $3 (the claiming worker's cost rank)  -  never on t  -  so
	 -- computing it per job is provably identical to the old per-task value (proven
	 -- by TestClaimCheaperClassOnlinePerJobEquivalence: the two orderings produce
	 -- byte-identical full ordered task lists at every queue position).
	 eligible_jobs AS MATERIALIZED (
	   SELECT j.id AS job_id, j.tier, j.job_type, j.model_ref,
	     me.worker_id AS claim_worker_id,
	     me.supplier_id_s AS claim_supplier_id,
	     me.hw_class AS claim_hw_class,
		     me.engine AS claim_engine,
		     me.build_hash AS claim_build_hash,
		     me.priority_claim_streak,
	     runtime_authority.cell_id AS runtime_cell_id,
	     runtime_authority.runtime_id AS runtime_id,
	     runtime_authority.matrix_sha256 AS runtime_matrix_sha256,
	     runtime_authority.model_kind AS model_kind,
	     -- Hardware-matched routing (cost preference, NOT a filter): is a strictly
	     -- CHEAPER hardware class than THIS claiming worker ($3 = its hwClassCostRank)
	     -- online AND eligible for this job right now? "Eligible" reuses the EXACT
	     -- hard-filter predicates this claim applies (live <60s, supplier active, not
	     -- throttled, enough effective/total memory, the job's hw_classes pin,
	     -- exact current-matrix capability), so we only defer a task a cheaper
	     -- class could ACTUALLY take. When true, an expensive worker steps back (ORDER
	     -- BY below sorts these last) and lets the cheaper class grab it. A small embed
	     -- job never ties up an Ultra while a Base worker is idle. If no cheaper eligible
	     -- class is online, this is false and the
	     -- task ranks normally: the job is still claimed here (the cap is "cheapest
	     -- SUFFICIENT class", never starvation). SKIP LOCKED + the hard filter are
	     -- untouched  -  this only re-orders tasks the claiming worker already passes.
	     EXISTS (
	       SELECT 1 FROM workers w2
	         JOIN suppliers s2 ON s2.id = w2.supplier_id
	       WHERE w2.id <> me.worker_id
	         AND w2.last_seen_at IS NOT NULL
	         AND w2.last_seen_at > now() - interval '60 seconds'
	         AND s2.status = 'active'
	         AND NOT COALESCE(w2.throttled, false)
	         AND (`+hwClassCostRankSQL("w2.hw_class")+`) < $3
	         AND COALESCE(j.min_memory_gb,0) <= COALESCE(w2.effective_memory_gb, w2.memory_gb, 0)
	         AND (j.hw_classes IS NULL OR w2.hw_class = ANY(j.hw_classes))
	         AND EXISTS (
	           SELECT 1 FROM worker_authorized_capabilities wac2
	            WHERE wac2.worker_id = w2.id
	              AND wac2.job_type = j.job_type
	              AND wac2.model_ref = COALESCE(j.model_ref,'')
	              AND wac2.matrix_sha256 = $4
	         )
	     ) AS cheaper_class_online,
	     -- Dispatch-interleave fairness (Scheduling & Matching Engine 6.5->7,
	     -- docs/internal/CREED_AND_PATH_TO_TEN.md): how many of THIS job's own
	     -- tasks have already been dispatched (running or complete). Ordered
	     -- ASC in the claim's ORDER BY, just above the final oldest-first
	     -- tiebreak: a job that has already had many tasks served steps back so a
	     -- smaller/newer job's tasks interleave instead of waiting out a giant
	     -- job's entire multi-thousand-task backlog first, purely on age.
	     --
	     -- PATCH (Control plane hot path 8->9): this is a per-JOB count (it depends
	     -- only on t.job_id, identical for every task of the job), so it lives HERE
	     -- in eligible_jobs  -  computed ONCE per candidate job  -  not re-counted per
	     -- task in the next CTE below. At a 12k-task/240-job backlog that is 240
	     -- count subqueries instead of 12,000, the second-largest per-queue cost
	     -- after cheaper_class_online (measured: it is what kept the loaded:near-
	     -- empty ratio high once the fleet scan was already per-job). Selection
	     -- only  -  it re-orders tasks the worker already passes; eligibility unchanged.
	     (SELECT count(*) FROM tasks jt
	        WHERE jt.job_id = j.id AND jt.status IN ('running','complete')
	     ) AS job_dispatched_count,
	     -- Throughput tiebreak (selection only, determinism-SAFE): THIS worker's most
	     -- recent measured tokens/sec FOR THIS JOB's job type, from the benchmark the
	     -- agent reports (maintained in worker_tps_cache by UpsertWorker, read as a
	     -- plain indexed lookup). PATCH (Control plane hot path 8->9): job_type is a
	     -- per-JOB attribute and $1 is the constant claiming worker, so this is per
	     -- job  -  it moves here (looked up once per candidate job) rather than a
	     -- LEFT JOIN re-evaluated for every one of the 12k candidate tasks below.
	     (SELECT COALESCE(wtc.tps, 0) FROM worker_tps_cache wtc
	        WHERE wtc.worker_id = $1 AND wtc.job_type = j.job_type
	     ) AS worker_tps,
	     -- Warm-model tiebreak (P-warmtiebreak, "Scheduling & matching engine" 8->9):
	     -- is THIS JOB's model already loaded warm on the claiming worker ($1)? A
	     -- worker with the model warm wins a tie over one that would pay a cold load.
	     -- PATCH (Control plane hot path 8->9): model_ref is per-JOB and $1 is
	     -- constant, so this too is per job  -  computed once per candidate job, not
	     -- per candidate task.
	     (COALESCE(j.model_ref,'') <> '' AND EXISTS (
	       SELECT 1 FROM worker_model_state wms
	         WHERE wms.worker_id = $1 AND wms.model_id = j.model_ref
	           AND wms.last_seen_warm > now() - interval '60 seconds'
	     )) AS warm_for_task
	   FROM jobs j
	   CROSS JOIN me
	   -- Resolve the ONE exact server-authorized runtime tuple that will be frozen
	   -- onto the claimed task. LIMIT 1 is deterministic defense-in-depth if a future
	   -- matrix accidentally contains two cells for the same worker/job/model; the
	   -- registration projection remains the authority and no self-declared dispatch
	   -- field participates. The model kind is generated from the canonical matrix and
	   -- persisted with this exact registration row; the mutable DB model catalog is
	   -- pricing/resolver metadata, never dispatch authority.
	   CROSS JOIN LATERAL (
	     SELECT wac.cell_id, wac.runtime_id, wac.matrix_sha256, wac.model_kind
	       FROM worker_authorized_capabilities wac
	      WHERE wac.worker_id = me.worker_id
	        AND wac.job_type = j.job_type
	        AND wac.model_ref = COALESCE(j.model_ref,'')
	        AND wac.matrix_sha256 = $4
	      ORDER BY wac.cell_id, wac.runtime_id
	      LIMIT 1
	   ) runtime_authority
	   WHERE j.status NOT IN ('cancelled','failed')
	     AND runtime_authority.model_kind <> ''
	     -- Only jobs that ACTUALLY have a claimable task right now reach here. A job
	     -- with no queued/retrying-and-visible task this worker could take can never
	     -- produce a next-CTE row (the tasks join below filters it out anyway), so
	     -- pre-restricting eligible_jobs to jobs-with-work keeps the result set
	     -- identical while skipping the per-job cheaper_class_online fleet scan for
	     -- every finished job (the jobs table keeps every completed job forever).
	     -- without this guard the claim would re-price the whole hardware fleet against
	     -- every historical job on every claim). The guard admits a task that is either
	     -- unclaimed OR pinned-to-THIS-worker-and-unstarted, covering BOTH claim
	     -- branches (the spliced claimed_by predicate below) so the pinned/hedge
	     -- branch is never starved of its job. Index-served by
	     -- tasks_ready_unclaimed_idx (unclaimed) / tasks_pkey.
	     AND EXISTS (
	           SELECT 1 FROM tasks tt
	            WHERE tt.job_id = j.id
	              AND tt.status IN ('queued','retrying')
	              AND (tt.claimed_by IS NULL
	                   OR (tt.claimed_by = $1 AND tt.started_at IS NULL))
	              AND COALESCE(tt.visible_at, tt.created_at) <= now())
	     AND me.supplier_status = 'active'
	     -- SAFE memory: effective allocatable (after the supplier's reserved
	     -- headroom) once the worker has heartbeated it, else total memory_gb
	     -- (a just-registered worker is unchanged  -  no regression).
	     AND COALESCE(j.min_memory_gb,0) <= COALESCE(me.effective_memory_gb, me.memory_gb, 0)
	     -- never hand work to a worker pausing for memory pressure.
	     AND NOT COALESCE(me.throttled, false)
	     AND (j.hw_classes IS NULL OR me.hw_class = ANY(j.hw_classes))
	     -- The LATERAL runtime_authority join above is the exact-cell hard filter.
	     -- Array-only legacy workers and stale matrix rows produce no eligible row.
	     AND (j.data_residency IS NULL OR me.data_country = ANY(j.data_residency))
	     -- Elite-supplier gate (DEEP_RESEARCH_V2 §6.4 anti-defection): a high
	     -- min_reputation job is claimable only by a supplier who earned that
	     -- reputation on the platform (0 = any supplier; the unchanged path).
	     AND COALESCE(j.min_reputation,0) <= me.reputation
	     AND (j.tier <> 'trusted' OR $2 >= 2)
	     AND (COALESCE(j.offered_rate_usd_hr,1e9) >= COALESCE(me.min_payout_usd_hr,0))
	     -- Budget Governor (Plane C §12 / Plane D §14 D8): when the job has a hard
	     -- spend cap, NEVER dispatch a new task whose projected charge would breach
	     -- it. Projected = already-charged on this job's tasks (buyer_charge debits,
	     -- same ledger shape failJobAndSettleOnce settles from) + the immutable frozen
	     -- charge for every IN-FLIGHT (claimed, running, not-yet-committed) task
	     -- (each will charge at commit, so it is exposure-in-flight) + ONE more for
	     -- the candidate + the once-only SLA premium exposure. Counting in-flight
	     -- work is what makes the cap hold under
	     -- the agent's bounded concurrency ([2,4] permits): without it, several
	     -- tasks could be claimed+running but uncommitted (contributing zero charged)
	     -- and each then charge past the cap. A capped+exhausted job's tasks stay
	     -- queued (the cap PREVENTS dispatch  -  it never refunds); jobs with no cap
	     -- are unaffected (j.max_usd IS NULL). This is a per-JOB predicate, so it too
	     -- moves here (evaluated once per job, not once per task).
	     AND (j.max_usd IS NULL OR (
	           (SELECT COALESCE(SUM(-le.amount_usd),0) FROM ledger_entries le
	            WHERE le.kind = 'buyer_charge'
	              AND le.task_id IN (SELECT id FROM tasks WHERE job_id = j.id))
	           + (SELECT COUNT(*) FROM tasks it
	                WHERE it.job_id = j.id AND it.status IN ('running','verifying'))
	             * (SELECT p.buyer_charge_per_task_usd
	                  FROM job_economic_plans p WHERE p.job_id=j.id)
	           + (SELECT p.buyer_charge_per_task_usd + p.sla_premium_usd
	                FROM job_economic_plans p WHERE p.job_id=j.id)
	         ) <= j.max_usd)
	 ),
	 next AS (
	   -- Every ORDER BY signal (cheaper_class_online, worker_tps, warm_for_task,
	   -- job_dispatched_count, tier) is PER-JOB  -  computed once per candidate job in
	   -- eligible_jobs above and carried on ej. The ONLY genuinely per-task columns
	   -- are t.id, t.created_at, and t.claimed_by. So this CTE is a lean tasks scan
	   -- joined to the (small) eligible_jobs set: no per-task subquery, no per-task
	   -- fleet scan, no per-task benchmark lookup. PATCH (Control plane hot path
	   -- 8->9): moving worker_tps / warm_for_task / job_dispatched_count up into
	   -- eligible_jobs (all per-job) collapsed the remaining O(queue) work here to
	   -- just the tasks scan + the LIMIT-1 sort, which is what flattens the
	   -- loaded:near-empty claim-latency ratio (measured: entry 61's ~190x, and
	   -- ~21x with only cheaper_class_online hoisted, down to a small multiple once
	   -- all four per-job signals are hoisted).
	   SELECT t.id,
	     ej.cheaper_class_online,
	     ej.worker_tps,
	     ej.warm_for_task,
	     ej.job_dispatched_count,
	     ej.runtime_cell_id,
	     ej.runtime_id,
	     ej.runtime_matrix_sha256,
	     ej.model_kind,
	     ej.claim_worker_id,
	     ej.claim_supplier_id,
	     ej.claim_hw_class,
	     ej.claim_engine,
	     ej.claim_build_hash
	   FROM tasks t
	     -- Only the per-JOB-eligible jobs survive to here (eligible_jobs above), so
	     -- this join IS the whole hard filter except the two per-task conditions
	     -- below. Every ordering signal + tier rides along on ej, already computed
	     -- once per job.
	     JOIN eligible_jobs ej ON ej.job_id = t.job_id
	   WHERE t.status IN ('queued','retrying')
	     AND COALESCE(t.visible_at, t.created_at) <= now()
	     -- claimable when unclaimed, OR pre-claimed (pinned) to THIS worker and
	     -- not yet started (a tiebreak/hedge dispatch pinned to a chosen peer).
	     -- PATCH (P-splitclaim, docs/CREED_AND_PATH_TO_TEN.md "Control plane hot
	     -- path" 6->7): this used to be one OR'd condition
	     -- (t.claimed_by IS NULL OR (t.claimed_by = $1 AND t.started_at IS NULL)),
	     -- which the query planner cannot serve from tasks_ready_unclaimed_idx
	     -- (a partial index WHERE claimed_by IS NULL only) because the second
	     -- OR-branch needs rows the index does not contain. The token below is
	     -- substituted by the Go caller with ONE of two plain, non-parameterized
	     -- literals  -  the
	     -- pinned branch tried first, then the plain "claimed_by IS NULL" branch.
	     -- so the common (unclaimed) case is a direct, index-servable predicate
	     -- instead of a planner-defeating OR. These are the ONLY genuinely
	     -- per-task conditions; every other filter is per-job (eligible_jobs above).
	     AND (%s)
	     -- Verification-requeue worker exclusion (Scheduling & Matching Engine 8->9,
	     -- docs/internal/CREED_AND_PATH_TO_TEN.md): a task that just failed a honeypot
	     -- was requeued with excluded_worker = the worker that failed it, in force
	     -- until excluded_until. Skip that worker for the window so a DIFFERENT worker
	     -- gets first crack; once the window elapses (or for any other worker) the
	     -- task is claimable normally, so a thin/single-worker fleet is never
	     -- permanently starved of the retry. Genuinely per-task (references $1)  -  it
	     -- belongs here, not in eligible_jobs.
	     AND (t.excluded_worker IS NULL
	          OR t.excluded_worker <> $1
	          OR t.excluded_until IS NULL
	          OR t.excluded_until <= now())
	     -- A redundancy row carrying hedged_from is a third-opinion task. Its
	     -- verification class was frozen when the disagreement was planned, and
	     -- every worker/supplier that has already executed this chunk is permanently
	     -- ineligible. This predicate applies to BOTH the pinned and general claim
	     -- branches: a profile/capability change cannot let an unsafe pin start, and
	     -- FailTaskTx/the stale reaper may clear the pin without handing a disputant
	     -- (or another machine owned by that supplier) the third vote.
	     AND (
	       NOT (COALESCE(t.is_redundancy,false) AND t.hedged_from IS NOT NULL)
	       OR (
	         NULLIF(COALESCE(t.verification_hw_class,''),'') IS NOT NULL
	         AND ej.claim_hw_class=t.verification_hw_class
	         AND (COALESCE(t.verification_engine,'')=''
	              OR ej.claim_engine=t.verification_engine)
	         AND (COALESCE(t.verification_build_hash,'')=''
	              OR ej.claim_build_hash=t.verification_build_hash)
	         AND NOT EXISTS (
	           SELECT 1
	             FROM (
	               -- Current durable task projections cover pre-history rows and
	               -- completed disputants whose worker_id remains attached.
	               SELECT prior.execution_worker_id AS worker_id,
	                      prior.execution_supplier_id AS supplier_id
	                 FROM tasks prior
	                WHERE prior.job_id=t.job_id
	                  AND COALESCE(prior.chunk_index,0)=COALESCE(t.chunk_index,0)
	                  AND prior.id<>t.id AND prior.execution_worker_id IS NOT NULL
	               UNION ALL
	               -- Durable commit snapshots freeze worker AND supplier identity;
	               -- unlike a mutable worker profile, this survives retries and
	               -- any later administrative supplier reassignment.
	               SELECT work.worker_id,work.supplier_id
	                 FROM verification_work work
	                 JOIN tasks committed ON committed.id=work.task_id
	                WHERE committed.job_id=t.job_id
	                  AND COALESCE(committed.chunk_index,0)=COALESCE(t.chunk_index,0)
	               UNION ALL
	               -- Claim history survives every retry path that clears worker_id.
	               SELECT history.worker_id,history.supplier_id
	                 FROM task_execution_history history
	                 JOIN tasks attempted ON attempted.id=history.task_id
	                WHERE attempted.job_id=t.job_id
	                  AND COALESCE(attempted.chunk_index,0)=COALESCE(t.chunk_index,0)
	             ) executed
	            WHERE executed.worker_id=ej.claim_worker_id
	               OR executed.supplier_id=ej.claim_supplier_id
	         )
	       )
	     )
		   -- Pinned-to-this-worker jumps the line. For ordinary claims, priority wins
		   -- until this worker has taken three consecutive priority tasks; while that
		   -- durable debt is outstanding an eligible batch task wins one opportunity
		   -- and resets the streak. If no batch task is eligible, priority may continue
		   -- without idling the worker while the streak remains capped at three.
		   -- After service-lane fairness, apply the
		   -- hardware-matched routing preference: within a tier, prefer tasks NO cheaper
	   -- eligible class is online for (cheaper_class_online = false sorts first), so
	   -- an expensive worker defers work a cheaper idle class could take instead of
	   -- tying itself up on it; THEN the throughput tiebreak (this worker's measured
	   -- tokens/sec for the task's job type); THEN warm_for_task (PATCH
	   -- P-warmtiebreak  -  replaces the old constant-per-query $4::real bw_gbps
	   -- no-op with a real per-row "is the task's model already loaded on this
	   -- worker" check, so a warm worker wins a tie over one that would pay a
	   -- cold load); THEN job_dispatched_count ASC (PATCH P-fairness, Scheduling &
	   -- Matching Engine 6.5->7): a job that has already had many of its OWN tasks
	   -- served steps back so a smaller/newer job's tasks interleave, instead of a
	   -- single multi-thousand-task job monopolizing every worker purely by being
	   -- older; finally oldest-first as the last tiebreak. These preference terms
		   -- all sit BELOW pin + service-lane fairness on purpose  -  a hedge/tiebreak
		   -- pinned here is still served promptly, and ordinary priority retains a strict
		   -- three-to-one preference whenever batch work is simultaneously eligible.
		   -- Those jobs are still served promptly even by an expensive, slower,
		   -- cold, or already-well-served-job worker. Every term below pin+priority is
	   -- selection only (no output change): it only re-orders tasks the worker
	   -- already passes; the hard filter above and SKIP LOCKED are unchanged. NB:
	   -- (t.claimed_by = $1) is a constant within each branch (the WHERE above pins
	   -- it), so it sorts consistently exactly as before the eligible_jobs split.
		   ORDER BY (t.claimed_by = $1) DESC,
		            CASE WHEN ej.priority_claim_streak >= 3
		                 THEN (ej.tier = 'batch')
		                 ELSE (ej.tier = 'priority')
		            END DESC,
		            (ej.tier = 'priority') DESC,
		            cheaper_class_online ASC, worker_tps DESC, warm_for_task DESC,
	            job_dispatched_count ASC, t.created_at ASC
	   FOR UPDATE OF t SKIP LOCKED
	   LIMIT 1
	 )
	 UPDATE tasks
	   SET claimed_by = $1, claimed_at = now(), worker_id = $1,
	       status = 'running', started_at = now(),
	       result_key = 'jobs/' || tasks.job_id::text || '/tasks/' || tasks.id::text ||
	                    '/attempt-' || COALESCE(tasks.retry_count,0)::text || '/result.json',
	       runtime_cell_id = next.runtime_cell_id,
	       runtime_id = next.runtime_id,
	       runtime_matrix_sha256 = next.runtime_matrix_sha256,
	       model_kind = next.model_kind,
	       execution_worker_id = next.claim_worker_id,
	       execution_supplier_id = next.claim_supplier_id,
	       execution_hw_class = next.claim_hw_class,
	       execution_engine = next.claim_engine,
	       execution_build_hash = next.claim_build_hash
	 FROM next, jobs j
	 WHERE tasks.id = next.id AND j.id = tasks.job_id
	 RETURNING tasks.id, COALESCE(tasks.retry_count,0), tasks.job_id, j.job_type, COALESCE(j.model_ref,''),
	           tasks.model_kind, tasks.runtime_cell_id, tasks.runtime_id,
	           tasks.runtime_matrix_sha256,
		           COALESCE(tasks.input_ref,''), COALESCE(tasks.result_key,''),
		           COALESCE(j.output_ref,''), j.tier,
		           COALESCE(j.min_memory_gb,0), j.hw_classes,
		           COALESCE(j.max_duration_secs,0), j.data_residency,
		           COALESCE(j.verification_policy,'{}'::jsonb),
	           COALESCE(j.job_type_spec,'null'::jsonb),
	           COALESCE(j.offered_rate_usd_hr,0), COALESCE(tasks.chunk_index,0),
	           tasks.is_honeypot`,
		claimedByPredicate)
}

func (s *Store) ClaimTasksTx(ctx context.Context, w WorkerAuth) (*ClaimedTask, error) {
	claimStart := time.Now()
	defer func() { claimDuration.observe(time.Since(claimStart)) }()

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	var hwClass string
	if err := tx.QueryRow(ctx,
		`SELECT COALESCE(hw_class,'') FROM workers WHERE id = $1 FOR UPDATE`, w.WorkerID,
	).Scan(&hwClass); errors.Is(err, pgx.ErrNoRows) {
		return nil, errNotFound
	} else if err != nil {
		return nil, err
	}
	selfCostRank := hwClassCostRank(hwClass)

	var rep float32
	var jobsDone uint64
	if err := tx.QueryRow(ctx,
		`SELECT s.reputation, s.completed_tasks FROM suppliers s WHERE s.id = $1`,
		w.SupplierID,
	).Scan(&rep, &jobsDone); err != nil {
		return nil, err
	}
	tier := reputationTier(rep, jobsDone)

	var c ClaimedTask
	claimTaskQuery := ClaimTaskSQL
	scanClaim := func(claimedByPredicate string) error {
		return tx.QueryRow(ctx, claimTaskQuery(claimedByPredicate),
			w.WorkerID, int(tier), selfCostRank, generatedRuntimeMatrixSHA256,
		).Scan(&c.TaskID, &c.Attempt, &c.JobID, &c.JobType, &c.ModelRef, &c.ModelKind,
			&c.RuntimeCellID, &c.RuntimeID, &c.RuntimeMatrixSHA, &c.InputRef, &c.ResultKey,
			&c.OutputRef, &c.Tier, &c.MinMemoryGB, &c.HWClasses, &c.MaxDurationSecs,
			&c.DataResidency, &c.VerifPolicy, &c.JobTypeSpec, &c.OfferedRateUsdHr,
			&c.ChunkIndex, &c.IsHoneypot)
	}
	pinnedClaim := true
	err = scanClaim("t.claimed_by = $1 AND t.started_at IS NULL")
	if errors.Is(err, pgx.ErrNoRows) {
		pinnedClaim = false
		err = scanClaim("t.claimed_by IS NULL")
	}
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	claimed := err == nil
	if claimed {
		if err := validateTaskAttemptResultKey(c.JobID, c.TaskID, c.Attempt, c.ResultKey); err != nil {
			return nil, fmt.Errorf("claim persisted a non-canonical staging key: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO task_execution_history (task_id,attempt,worker_id,supplier_id)
			SELECT t.id,COALESCE(t.retry_count,0),t.execution_worker_id,t.execution_supplier_id
			  FROM tasks t
			 WHERE t.id=$1 AND t.is_redundancy=true AND t.hedged_from IS NOT NULL
			ON CONFLICT (task_id,attempt,worker_id) DO NOTHING`, c.TaskID); err != nil {
			return nil, err
		}
		var lockedJob uuid.UUID
		if err := tx.QueryRow(ctx,
			`SELECT id FROM jobs WHERE id=$1 FOR UPDATE`, c.JobID,
		).Scan(&lockedJob); err != nil {
			return nil, err
		}
		var withinCap bool
		if err := tx.QueryRow(ctx, `
			SELECT j.max_usd IS NULL OR COALESCE((
			  (SELECT COALESCE(SUM(-le.amount_usd),0)
			     FROM ledger_entries le
			    WHERE le.kind='buyer_charge'
			      AND le.task_id IN (SELECT id FROM tasks WHERE job_id=j.id))
			  + (SELECT COUNT(*) FROM tasks t
			       WHERE t.job_id=j.id AND t.status IN ('running','verifying'))
			    * (SELECT p.buyer_charge_per_task_usd
			         FROM job_economic_plans p WHERE p.job_id=j.id)
			  + (SELECT p.sla_premium_usd FROM job_economic_plans p WHERE p.job_id=j.id)
			) <= j.max_usd, false)
			FROM jobs j WHERE j.id=$1`, c.JobID).Scan(&withinCap); err != nil {
			return nil, err
		}
		if !withinCap {
			return nil, nil // deferred rollback restores the candidate's prior queue/pin state
		}

		if _, err := tx.Exec(ctx,
			`UPDATE jobs SET status = 'running' WHERE id = $1 AND status = 'queued'`,
			c.JobID); err != nil {
			return nil, err
		}
		if err := budgetWarnOnDispatch(ctx, tx, c.JobID); err != nil {
			return nil, err
		}
		if !pinnedClaim {
			if _, err := tx.Exec(ctx, `
				UPDATE workers
				   SET priority_claim_streak = CASE
				         WHEN $2 = 'priority' THEN LEAST(priority_claim_streak + 1, 3)
				         WHEN $2 = 'batch' THEN 0
				         ELSE priority_claim_streak
				       END
				 WHERE id = $1`, w.WorkerID, c.Tier); err != nil {
				return nil, err
			}
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	if !claimed {
		return nil, nil // no work available -> 204
	}
	return &c, nil
}

type SchedulerExplanation struct {
	WorkerID          uuid.UUID `json:"worker_id"`
	NoQueuedTasks     int       `json:"no_queued_tasks"`    // no claimable task exists at all (empty queue)
	MemoryMismatch    int       `json:"memory_mismatch"`    // job min_memory_gb > worker effective (or total) memory
	ModelMismatch     int       `json:"model_mismatch"`     // job needs a model the worker has not loaded
	JobTypeMismatch   int       `json:"job_type_mismatch"`  // worker does not support the job's type
	HWClassMismatch   int       `json:"hw_class_mismatch"`  // job pins hw_classes the worker is not in
	ResidencyMismatch int       `json:"residency_mismatch"` // supplier's data_country not in job's data_residency
	Throttled         int       `json:"throttled"`          // worker is pausing for memory pressure
	PayoutFloor       int       `json:"payout_floor"`       // economic/trust gate: offered rate below the worker's floor, OR a trusted-tier job the supplier's tier (<2) cannot take
	SupplierInactive  int       `json:"supplier_inactive"`  // supplier not active (quarantine/suspension gate)
	Eligible          int       `json:"eligible"`           // tasks this worker COULD claim right now
}

func (s *Store) SchedulerExplain(ctx context.Context, workerID uuid.UUID) (*SchedulerExplanation, error) {
	var exists bool
	if err := s.pool.QueryRow(ctx,
		`SELECT true FROM workers WHERE id = $1`, workerID,
	).Scan(&exists); errors.Is(err, pgx.ErrNoRows) {
		return nil, errNotFound
	} else if err != nil {
		return nil, err
	}

	var rep float32
	var jobsDone uint64
	if err := s.pool.QueryRow(ctx,
		`SELECT s.reputation, s.completed_tasks
		 FROM suppliers s
		 WHERE s.id = (SELECT supplier_id FROM workers WHERE id = $1)`,
		workerID,
	).Scan(&rep, &jobsDone); err != nil {
		return nil, err
	}
	tier := reputationTier(rep, jobsDone)

	e := SchedulerExplanation{WorkerID: workerID}
	err := s.pool.QueryRow(ctx,
		`WITH w AS (SELECT * FROM workers WHERE id = $1),
		      claimable AS (
		        SELECT j.min_memory_gb, j.hw_classes, j.data_residency, j.job_type,
		               j.model_ref, j.offered_rate_usd_hr, j.tier AS job_tier, j.min_reputation,
		               j.buyer_id
		        FROM tasks t
		          JOIN jobs j ON j.id = t.job_id
		        WHERE t.status IN ('queued','retrying')
		          AND COALESCE(t.visible_at, t.created_at) <= now()
		          AND (t.claimed_by IS NULL OR (t.claimed_by = $1 AND t.started_at IS NULL))
		          AND j.status NOT IN ('cancelled','failed')
		      ),
		      classified AS (
		        SELECT CASE
		          WHEN s.status <> 'active' THEN 'supplier_inactive'
		          WHEN COALESCE(c.min_memory_gb,0) > COALESCE(w.effective_memory_gb, w.memory_gb, 0) THEN 'memory_mismatch'
		          WHEN COALESCE(w.throttled, false) THEN 'throttled'
		          WHEN NOT (c.hw_classes IS NULL OR w.hw_class = ANY(c.hw_classes)) THEN 'hw_class_mismatch'
		          WHEN NOT EXISTS (
		            SELECT 1 FROM worker_authorized_capabilities wac_job
		             WHERE wac_job.worker_id = w.id
		               AND wac_job.job_type = c.job_type
		               AND wac_job.matrix_sha256 = $3
		          ) THEN 'job_type_mismatch'
		          WHEN NOT EXISTS (
		            SELECT 1 FROM worker_authorized_capabilities wac
		             WHERE wac.worker_id = w.id
		               AND wac.job_type = c.job_type
		               AND wac.model_ref = COALESCE(c.model_ref,'')
		               AND wac.matrix_sha256 = $3
		          ) THEN 'model_mismatch'
		          WHEN NOT (c.data_residency IS NULL OR s.data_country = ANY(c.data_residency)) THEN 'residency_mismatch'
		          WHEN COALESCE(c.min_reputation,0) > s.reputation THEN 'reputation_too_low'
		          WHEN NOT (c.job_tier <> 'trusted' OR $2 >= 2) THEN 'tier_gate'
		          WHEN NOT (COALESCE(c.offered_rate_usd_hr,1e9) >= COALESCE(w.min_payout_usd_hr,0)) THEN 'payout_floor'
		          ELSE 'eligible'
		        END AS reason
		        FROM claimable c, w
		          JOIN suppliers s ON s.id = w.supplier_id
		      )
		 SELECT
		   count(*) FILTER (WHERE reason = 'memory_mismatch'),
		   count(*) FILTER (WHERE reason = 'model_mismatch'),
		   count(*) FILTER (WHERE reason = 'job_type_mismatch'),
		   count(*) FILTER (WHERE reason = 'hw_class_mismatch'),
		   count(*) FILTER (WHERE reason = 'residency_mismatch'),
		   count(*) FILTER (WHERE reason = 'throttled'),
		   count(*) FILTER (WHERE reason = 'payout_floor' OR reason = 'tier_gate' OR reason = 'reputation_too_low'),
		   count(*) FILTER (WHERE reason = 'supplier_inactive'),
		   count(*) FILTER (WHERE reason = 'eligible')
		 FROM classified`,
		workerID, int(tier), generatedRuntimeMatrixSHA256,
	).Scan(&e.MemoryMismatch, &e.ModelMismatch, &e.JobTypeMismatch, &e.HWClassMismatch,
		&e.ResidencyMismatch, &e.Throttled, &e.PayoutFloor, &e.SupplierInactive, &e.Eligible)
	if err != nil {
		return nil, err
	}
	if e.MemoryMismatch+e.ModelMismatch+e.JobTypeMismatch+e.HWClassMismatch+
		e.ResidencyMismatch+e.Throttled+e.PayoutFloor+e.SupplierInactive+e.Eligible == 0 {
		e.NoQueuedTasks = 1
	}
	return &e, nil
}

const budgetThresholdFrac = 0.80

const budgetProjectedExpr = `(
   (SELECT COALESCE(SUM(-le.amount_usd),0) FROM ledger_entries le
    WHERE le.kind = 'buyer_charge'
      AND le.task_id IN (SELECT id FROM tasks WHERE job_id = j.id))
   + (SELECT COUNT(*) FROM tasks it
        WHERE it.job_id = j.id AND it.status IN ('running','verifying'))
     * (SELECT p.buyer_charge_per_task_usd
          FROM job_economic_plans p WHERE p.job_id=j.id)
   + (SELECT p.buyer_charge_per_task_usd + p.sla_premium_usd
        FROM job_economic_plans p WHERE p.job_id=j.id)
 )`

func budgetWarnOnDispatch(ctx context.Context, tx pgx.Tx, jobID uuid.UUID) error {
	var crossed bool
	err := tx.QueryRow(ctx,
		`UPDATE jobs j
		   SET budget_state = 'near_limit'
		 WHERE j.id = $1
		   AND j.max_usd IS NOT NULL
		   AND j.budget_state = 'tracking'
		   AND `+budgetProjectedExpr+` >= $2 * j.max_usd
		 RETURNING true`,
		jobID, budgetThresholdFrac,
	).Scan(&crossed)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil // not capped, already past 'tracking', or below the threshold
	}
	if err != nil {
		return err
	}
	if crossed {
		return insertEventTx(ctx, tx, jobID, nil, "budget_warning",
			"Approaching your budget cap (>= 80% projected).", nil)
	}
	return nil
}

func (s *Store) SweepBudgetStops(ctx context.Context) (int, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	stopped, err := markBudgetStoppedJobs(ctx, tx)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return stopped, nil
}

func markBudgetStoppedJobs(ctx context.Context, tx pgx.Tx) (int, error) {
	rows, err := tx.Query(ctx,
		`UPDATE jobs j
		   SET budget_state = 'paused_for_budget'
		 WHERE j.max_usd IS NOT NULL
		   AND j.budget_state NOT IN ('paused_for_budget','cancelled_by_budget')
		   AND j.status NOT IN ('cancelled','failed','complete')
		   AND `+budgetProjectedExpr+` > j.max_usd
		   AND EXISTS (
		     SELECT 1 FROM tasks t
		     WHERE t.job_id = j.id
		       AND t.status IN ('queued','retrying')
		       AND t.claimed_by IS NULL
		       AND COALESCE(t.visible_at, t.created_at) <= now()
		   )
		 RETURNING j.id`)
	if err != nil {
		return 0, err
	}
	var paused []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		paused = append(paused, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	for _, id := range paused {
		if err := insertEventTx(ctx, tx, id, nil, "budget_stopped",
			"This job is paused before exceeding your budget cap.", nil); err != nil {
			return 0, err
		}
	}
	return len(paused), nil
}
