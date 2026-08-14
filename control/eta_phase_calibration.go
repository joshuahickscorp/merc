package main

import (
	"context"
	"errors"
	"fmt"
	"log"

	"github.com/google/uuid"
)

// Per-phase prediction lives on eta_calibration — the same surface that owns
// total-duration predicted-vs-actual. A second learner over duration would fork
// authority (see plan_actuals.go: duration is refused there for that reason).
//
// What changed in eta_calibration for G053:
//
//   - phase TEXT NOT NULL DEFAULT 'total' — legacy rows are total; new rows may
//     name a captured phase (queue, startup, …).
//   - subject_kind / subject_id — realtime contracts and leases are not jobs.
//   - predicted_ms / realized_ms — phase spans are milliseconds; the INT secs
//     columns stay for the legacy total-duration path.
//   - uniqueness is (job_id, phase) or (subject_kind, subject_id, phase).
//
// ETABiasFactor still reads ONLY phase='total' (and predicted_secs > 0). Phase
// rows are observations. They must never stretch a quoted ETA or influence
// admission, reserve, pricing, or selection.

// PhaseCalibrationRow is one predicted-vs-actual observation for a named phase.
// PredictedKnown false means there was no estimator for this phase at write
// time — the actual is still stored so coverage is visible, and regret over
// that row is refused rather than computed against zero.
type PhaseCalibrationRow struct {
	Phase          string  `json:"phase"`
	SubjectKind    string  `json:"subject_kind"`
	SubjectID      string  `json:"subject_id"`
	JobType        string  `json:"job_type,omitempty"`
	Tier           string  `json:"tier,omitempty"`
	ModelRef       string  `json:"model_ref,omitempty"`
	PredictedMS    float64 `json:"predicted_ms,omitempty"`
	PredictedKnown bool    `json:"predicted_known"`
	RealizedMS     float64 `json:"realized_ms,omitempty"`
	RealizedKnown  bool    `json:"realized_known"`
}

// recordJobPhaseCalibrations writes per-phase actuals for every task of a
// finalized job. Observation failure must never affect a finalize that already
// committed money.
func recordJobPhaseCalibrations(ctx context.Context, store *Store, jobID uuid.UUID) {
	if store == nil || jobID == uuid.Nil {
		return
	}
	rows, err := store.pool.Query(ctx, `SELECT id FROM tasks WHERE job_id = $1`, jobID)
	if err != nil {
		log.Printf("phase calibrations list tasks for job %s: %v (finalize unaffected)", jobID, err)
		return
	}
	defer rows.Close()
	for rows.Next() {
		var taskID uuid.UUID
		if err := rows.Scan(&taskID); err != nil {
			log.Printf("phase calibrations scan task for job %s: %v (finalize unaffected)", jobID, err)
			return
		}
		if err := store.RecordTaskPhaseCalibrations(ctx, jobID, taskID); err != nil {
			log.Printf("phase calibrations for task %s: %v (finalize unaffected)", taskID, err)
		}
	}
}

// RecordTaskPhaseCalibrations writes one eta_calibration row per known batch
// phase from DecomposeTaskPhases. Predicted is left NULL unless a prior
// phase='total' bias provides no per-phase estimator — we do not invent one.
//
// Total duration continues to be written by RecordEtaCalibration; this path
// never writes phase='total'.
func (s *Store) RecordTaskPhaseCalibrations(ctx context.Context, jobID, taskID uuid.UUID) error {
	if taskID == uuid.Nil {
		return errors.New("task phase calibration requires a task id")
	}
	decomp, err := DecomposeTaskPhases(ctx, s.pool, taskID)
	if err != nil {
		return err
	}
	var jobType, tier, modelRef string
	_ = s.pool.QueryRow(ctx, `
		SELECT COALESCE(job_type,''), COALESCE(tier,''), COALESCE(model_ref,'')
		  FROM jobs WHERE id = $1`, jobID).Scan(&jobType, &tier, &modelRef)

	for _, p := range decomp.KnownPhases() {
		if p.Name == "total" {
			continue // owned by RecordEtaCalibration
		}
		if err := s.insertPhaseCalibration(ctx, PhaseCalibrationRow{
			Phase: p.Name, SubjectKind: "task", SubjectID: taskID.String(),
			JobType: jobType, Tier: tier, ModelRef: modelRef,
			RealizedMS: p.DurationMS, RealizedKnown: true,
			// PredictedKnown stays false: no per-phase estimator exists yet.
		}, jobID); err != nil {
			return err
		}
	}
	return nil
}

// RecordRealtimePhaseCalibrations writes observed realtime TTFT phases for one
// execution. Prefill is skipped when unknown (always, under current protocol).
func (s *Store) RecordRealtimePhaseCalibrations(ctx context.Context, executionID uuid.UUID) error {
	decomp, err := DecomposeRealtimePhases(ctx, s.pool, executionID)
	if err != nil {
		return err
	}
	var jobType, modelRef string
	jobType = "CHAT_COMPLETION"
	_ = s.pool.QueryRow(ctx, `
		SELECT COALESCE(c.model_alias,'')
		  FROM execution_contracts c
		 WHERE c.id = $1`, decomp.ContractID).Scan(&modelRef)

	for _, p := range decomp.KnownPhases() {
		if err := s.insertPhaseCalibration(ctx, PhaseCalibrationRow{
			Phase: p.Name, SubjectKind: "execution_contract",
			SubjectID: decomp.ContractID.String(),
			JobType:   jobType, ModelRef: modelRef,
			RealizedMS: p.DurationMS, RealizedKnown: true,
		}, uuid.Nil); err != nil {
			return err
		}
	}
	return nil
}

// RecordLeasePhaseCalibrations writes observed lease phases. Only closed,
// known spans are inserted.
func (s *Store) RecordLeasePhaseCalibrations(ctx context.Context, leaseID uuid.UUID) error {
	decomp, err := DecomposeLeasePhases(ctx, s.pool, leaseID)
	if err != nil {
		return err
	}
	var modelRef string
	_ = s.pool.QueryRow(ctx, `
		SELECT COALESCE(runtime_profile_id,'') FROM service_leases WHERE id = $1`,
		leaseID).Scan(&modelRef)

	for _, p := range decomp.KnownPhases() {
		if err := s.insertPhaseCalibration(ctx, PhaseCalibrationRow{
			Phase: p.Name, SubjectKind: "service_lease",
			SubjectID: leaseID.String(),
			JobType:   "service_lease", ModelRef: modelRef,
			RealizedMS: p.DurationMS, RealizedKnown: true,
		}, uuid.Nil); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) insertPhaseCalibration(ctx context.Context, row PhaseCalibrationRow, jobID uuid.UUID) error {
	if row.Phase == "" || row.Phase == "total" {
		return fmt.Errorf("insertPhaseCalibration refuses phase %q (total is RecordEtaCalibration)", row.Phase)
	}
	if !row.RealizedKnown {
		// Do not insert a row that claims a realized value of zero for an
		// unobserved phase. Coverage for unknowns is the absence of a row.
		return nil
	}
	var predicted any
	if row.PredictedKnown {
		predicted = row.PredictedMS
	}
	var job any
	if jobID != uuid.Nil {
		job = jobID
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO eta_calibration
		  (job_id, job_type, tier, model_ref, phase, subject_kind, subject_id,
		   predicted_ms, realized_ms)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
		ON CONFLICT (subject_kind, subject_id, phase)
		 WHERE subject_id IS NOT NULL AND subject_kind IS NOT NULL
		 DO NOTHING`,
		job, phaseNullIfEmpty(row.JobType), phaseNullIfEmpty(row.Tier), phaseNullIfEmpty(row.ModelRef),
		row.Phase, row.SubjectKind, row.SubjectID, predicted, row.RealizedMS)
	return err
}

func phaseNullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// PhaseRegretMS is realized − predicted for one phase when both are known.
// Absent prediction or actual yields ok=false; never regret against zero.
func PhaseRegretMS(predicted, realized float64, predictedKnown, realizedKnown bool) (regret float64, ok bool) {
	if !predictedKnown || !realizedKnown {
		return 0, false
	}
	if predicted < 0 || realized < 0 {
		return 0, false
	}
	return realized - predicted, true
}
