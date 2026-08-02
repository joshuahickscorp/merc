package main

import (
	"context"
	"math"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Benchmark corroboration policy.
//
// A supplier's self-measured throughput must not become scheduling or
// admission authority when an independent observation of the same
// (job_type, model_id) cell class exists and disagrees. The party being paid
// has an incentive to inflate the number; a peer measurement is the check.
//
// Three states, not two:
//
//  1. Corroborated — a peer measurement agrees within tolerance. The claimed
//     rate is governed scheduling/admission authority.
//  2. Unpeered — no independent measurement of this cell class exists yet.
//     The cell is not "uncorroborated"; nothing could have corroborated it.
//     Penalising it with the floor makes a single-supplier (or first-of-class)
//     fleet unroutable and inverts cost rank against fixtures that correctly
//     advertise distinct measured rates. Unpeered cells keep the claimed rate
//     as provisional authority until a peer appears.
//  3. Disputed (uncorroborated with peers present) — at least one peer
//     measurement exists and none agrees. The scheduler and admission paths
//     see only the conservative floor, never the claimed rate.
//
// benchmarkCorroborationTolerance is a POLICY CHOICE, not a measurement.
// Justification: Metal thermal variance and batch-size noise on the retained
// MiniLM / Llama-1B benches in this repo routinely land within ~15% of a prior
// run on the same box; 25% leaves headroom for cross-machine class variance
// without accepting a 2x fabrication. Tighten only with a measured fleet
// variance study.
const benchmarkCorroborationTolerance = 0.25 // 25% relative

// uncorroboratedBenchmarkFloorTPS is the rate the scheduler and admission paths
// see when a cell class has peers and this self-report is not among the ones
// they agree with. It is deliberately too small to win a throughput tiebreak
// or clear a realistic payout floor on its own, so a disputed cell is not
// "routable at the claimed rate".
const uncorroboratedBenchmarkFloorTPS = 1.0

// ratesAgreeWithinTolerance reports whether two positive rates agree within
// the policy relative tolerance. Zero or non-finite rates never corroborate.
func ratesAgreeWithinTolerance(a, b, tolerance float64) bool {
	if a <= 0 || b <= 0 || math.IsNaN(a) || math.IsNaN(b) || math.IsInf(a, 0) || math.IsInf(b, 0) {
		return false
	}
	if tolerance < 0 {
		return false
	}
	base := math.Max(a, b)
	return math.Abs(a-b)/base <= tolerance
}

// tryCorroborateBenchmarkTx looks for an independent observation of the same
// cell class (different worker, preferably different supplier).
//
// Returns:
//   - corroborated=true, source set — a peer agrees within tolerance
//   - peerAvailable=true, corroborated=false — peers exist, none agree (disputed)
//   - peerAvailable=false — no peer measurement of this class (unpeered)
//
// Independent observation sources, in order of preference:
//  1. A second supplier's most recent measurement of the same (job_type, model)
//  2. A second worker of any supplier (weaker; still not the self-report alone)
//
// Honeypot/redundancy corroboration is applied separately when those tasks
// complete (see MarkBenchmarkCorroboratedFromVerification).
func tryCorroborateBenchmarkTx(
	ctx context.Context, tx pgx.Tx,
	workerID uuid.UUID, jobType, modelID string, claimedRate float32,
) (corroborated bool, peerAvailable bool, source string, err error) {
	if claimedRate <= 0 {
		return false, false, "", nil
	}
	// Prefer a different supplier. Scan recent peers so we can distinguish
	// "no peer" from "peer disagrees" rather than collapsing both to floor.
	rows, err := tx.Query(ctx, `
		SELECT COALESCE(br.claimed_rate, CASE WHEN br.job_type='embed' THEN br.eps ELSE br.tps END)
		  FROM benchmark_results br
		  JOIN workers w ON w.id = br.worker_id
		  JOIN workers me ON me.id = $1
		 WHERE br.job_type = $2 AND br.model_id = $3
		   AND br.worker_id <> $1
		   AND w.supplier_id IS DISTINCT FROM me.supplier_id
		   AND COALESCE(br.claimed_rate, CASE WHEN br.job_type='embed' THEN br.eps ELSE br.tps END) > 0
		 ORDER BY br.measured_at DESC
		 LIMIT 8`,
		workerID, jobType, modelID,
	)
	if err != nil {
		return false, false, "", err
	}
	var peerRates []float32
	for rows.Next() {
		var r float32
		if err := rows.Scan(&r); err != nil {
			rows.Close()
			return false, false, "", err
		}
		peerRates = append(peerRates, r)
	}
	if err := rows.Err(); err != nil {
		return false, false, "", err
	}
	rows.Close()
	for _, peerRate := range peerRates {
		peerAvailable = true
		if ratesAgreeWithinTolerance(float64(claimedRate), float64(peerRate), benchmarkCorroborationTolerance) {
			return true, true, "peer_supplier_measurement", nil
		}
	}

	// Fall back to a second worker (any supplier). Still independent of self.
	rows, err = tx.Query(ctx, `
		SELECT COALESCE(br.claimed_rate, CASE WHEN br.job_type='embed' THEN br.eps ELSE br.tps END)
		  FROM benchmark_results br
		 WHERE br.job_type = $2 AND br.model_id = $3
		   AND br.worker_id <> $1
		   AND COALESCE(br.claimed_rate, CASE WHEN br.job_type='embed' THEN br.eps ELSE br.tps END) > 0
		 ORDER BY br.measured_at DESC
		 LIMIT 8`,
		workerID, jobType, modelID,
	)
	if err != nil {
		return false, peerAvailable, "", err
	}
	defer rows.Close()
	for rows.Next() {
		var peerRate float32
		if err := rows.Scan(&peerRate); err != nil {
			return false, peerAvailable, "", err
		}
		peerAvailable = true
		if ratesAgreeWithinTolerance(float64(claimedRate), float64(peerRate), benchmarkCorroborationTolerance) {
			return true, true, "peer_worker_measurement", nil
		}
	}
	if err := rows.Err(); err != nil {
		return false, peerAvailable, "", err
	}
	return false, peerAvailable, "", nil
}

// RoutableBenchmarkRate is the rate a cell may be scheduled/priced at.
//
//	corroborated → claimed (if positive)
//	unpeered (!peerAvailable) → claimed as provisional authority
//	disputed (peerAvailable && !corroborated) → floor
func RoutableBenchmarkRate(claimed float32, corroborated bool, peerAvailable bool) float32 {
	if claimed <= 0 {
		return uncorroboratedBenchmarkFloorTPS
	}
	if corroborated || !peerAvailable {
		return claimed
	}
	return uncorroboratedBenchmarkFloorTPS
}

// MarkBenchmarkCorroboratedFromVerification records that an independent
// honeypot or redundancy observation has corroborated this worker's rate for
// the given cell class. Greppable source strings distinguish the path.
func (s *Store) MarkBenchmarkCorroboratedFromVerification(
	ctx context.Context, workerID uuid.UUID, jobType, modelID, source string,
) error {
	if source == "" {
		source = "verification_observation"
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `
		UPDATE benchmark_results
		   SET corroborated = true, corroboration_source = $2, corroborated_at = now()
		 WHERE id = (
		   SELECT id FROM benchmark_results
		    WHERE worker_id = $1 AND job_type = $3 AND model_id = $4
		    ORDER BY measured_at DESC LIMIT 1
		 ) AND corroborated = false`,
		workerID, source, jobType, modelID,
	); err != nil {
		return err
	}
	// Promote the cache to the claimed rate once corroborated.
	if _, err := tx.Exec(ctx, `
		UPDATE worker_tps_cache
		   SET tps = CASE WHEN claimed_tps > 0 THEN claimed_tps ELSE tps END,
		       corroborated = true, updated_at = now()
		 WHERE worker_id = $1 AND job_type = $2`,
		workerID, jobType,
	); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
