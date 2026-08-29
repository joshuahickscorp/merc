package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const (
	projectLoRAEvaluationReceiptVersion   = "MERC_LORA_EVALUATION_RECEIPT_V1"
	projectLoRAEvaluationReceiptStatus    = "EVALUATION_RECORDED_NOT_EXECUTABLE"
	projectLoRAEvaluationExecutionRefusal = "LoRA evaluation remains non-executable: no governed trainer, adapter deployment, buyer charge, supplier payable, or settlement authority is present"
)

var errProjectLoRAEvaluationReceiptNotFound = errors.New("project LoRA evaluation receipt not found")

// handleWorkerLoRAEvaluation is the production evaluator entry point. The
// authenticated worker supplies the evaluator identity; the store derives the
// supplier/account separation and binds the report to the probed LoRA IR.
func (s *Server) handleWorkerLoRAEvaluation(w http.ResponseWriter, r *http.Request) {
	auth := r.Context().Value(ctxWorker).(*WorkerAuth)
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxJSONRequestBodyBytes+1))
	if err != nil || len(raw) > maxJSONRequestBodyBytes {
		writeErr(w, http.StatusRequestEntityTooLarge, "LoRA evaluation exceeds the bounded JSON limit")
		return
	}
	var submission ProjectLoRAEvaluationSubmission
	if err := decodeStrictJSONObject(raw, &submission); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid LoRA evaluation: "+err.Error())
		return
	}
	receipt, err := s.store.RecordProjectLoRAEvaluation(r.Context(), *auth, submission)
	if errors.Is(err, errProjectLoRAEvaluationReceiptNotFound) {
		writeErr(w, http.StatusNotFound, "LoRA compile receipt or step not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusBadRequest, "LoRA evaluation refused: "+err.Error())
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusCreated, receipt)
}

func (s *Server) handleProjectLoRAEvaluationReceipt(w http.ResponseWriter, r *http.Request) {
	auth := r.Context().Value(ctxBuyer).(*AuthResult)
	receiptID, ok := pathUUID(w, r)
	if !ok {
		return
	}
	receipt, err := s.store.GetProjectLoRAEvaluationReceipt(r.Context(), auth.BuyerID, receiptID)
	if errors.Is(err, errProjectLoRAEvaluationReceiptNotFound) {
		writeErr(w, http.StatusNotFound, "LoRA evaluation receipt not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "LoRA evaluation receipt unavailable")
		return
	}
	writeJSON(w, http.StatusOK, receipt)
}

// ProjectLoRAEvaluationSubmission is the only report an evaluator worker may
// add to a probed LoRA compile receipt. The evaluator identity is taken from
// its authenticated worker token; it is deliberately not accepted in this
// body. The report is evidence only and cannot create an execution contract or
// a money obligation.
type ProjectLoRAEvaluationSubmission struct {
	CompileReceiptID     uuid.UUID `json:"compile_receipt_id"`
	StepID               string    `json:"step_id"`
	RunID                uuid.UUID `json:"run_id"`
	TrainerWorkerID      uuid.UUID `json:"trainer_worker_id"`
	AdapterSHA256        string    `json:"adapter_sha256"`
	HeldOutSetSHA256     string    `json:"held_out_set_sha256"`
	TrainerSawHeldOutSet bool      `json:"trainer_saw_held_out_set"`
	BaselineScore        float64   `json:"baseline_score"`
	CandidateScore       float64   `json:"candidate_score"`
}

// ProjectLoRAEvaluationReceipt is a durable, buyer-readable outcome report.
// Succeeded means only that the submitted scores cleared the declared margin;
// it never means that training, deployment, delivery, or settlement happened.
type ProjectLoRAEvaluationReceipt struct {
	ID                   uuid.UUID `json:"id"`
	Version              string    `json:"version"`
	CompileReceiptID     uuid.UUID `json:"compile_receipt_id"`
	ProjectSHA256        string    `json:"project_sha256"`
	IRSHA256             string    `json:"ir_sha256"`
	StepID               string    `json:"step_id"`
	RunID                uuid.UUID `json:"run_id"`
	TrainerWorkerID      uuid.UUID `json:"trainer_worker_id"`
	EvaluatorWorkerID    uuid.UUID `json:"evaluator_worker_id"`
	TrainerSupplierID    uuid.UUID `json:"trainer_supplier_id"`
	EvaluatorSupplierID  uuid.UUID `json:"evaluator_supplier_id"`
	TrainerAccountID     uuid.UUID `json:"trainer_account_id"`
	EvaluatorAccountID   uuid.UUID `json:"evaluator_account_id"`
	HeldOutSetSHA256     string    `json:"held_out_set_sha256"`
	ReservedSetSHA256    string    `json:"reserved_set_sha256"`
	AdapterSHA256        string    `json:"adapter_sha256"`
	BaselineModelSHA256  string    `json:"baseline_model_sha256"`
	EvaluationMetric     string    `json:"evaluation_metric"`
	MetricDirection      string    `json:"metric_direction"`
	BaselineScore        float64   `json:"baseline_score"`
	CandidateScore       float64   `json:"candidate_score"`
	RequiredImprovement  float64   `json:"required_improvement"`
	ImprovementFraction  float64   `json:"improvement_fraction"`
	TrainerSawHeldOutSet bool      `json:"trainer_saw_held_out_set"`
	Succeeded            bool      `json:"succeeded"`
	Status               string    `json:"status"`
	ExecutionRefusal     string    `json:"execution_refusal"`
	EvidenceSHA256       string    `json:"evidence_sha256"`
	CreatedAt            time.Time `json:"created_at"`
}

type projectLoRAEvaluationCompileBinding struct {
	ID            uuid.UUID
	BuyerID       uuid.UUID
	ProjectSHA256 string
	IRSHA256      string
	IR            ProjectWorkloadIR
	Step          ProjectIRStep
}

type projectLoRAWorkerIdentity struct {
	WorkerID   uuid.UUID
	SupplierID uuid.UUID
	AccountID  uuid.UUID
}

// loraEvaluationEvidence is the canonical, non-generated identity that is
// hashed into a receipt. It includes the buyer scope and all derived authority,
// so replaying the same run id with a changed report cannot silently overwrite
// the first observation.
type loraEvaluationEvidence struct {
	BuyerID              uuid.UUID `json:"buyer_id"`
	CompileReceiptID     uuid.UUID `json:"compile_receipt_id"`
	ProjectSHA256        string    `json:"project_sha256"`
	IRSHA256             string    `json:"ir_sha256"`
	StepID               string    `json:"step_id"`
	RunID                uuid.UUID `json:"run_id"`
	TrainerWorkerID      uuid.UUID `json:"trainer_worker_id"`
	EvaluatorWorkerID    uuid.UUID `json:"evaluator_worker_id"`
	TrainerSupplierID    uuid.UUID `json:"trainer_supplier_id"`
	EvaluatorSupplierID  uuid.UUID `json:"evaluator_supplier_id"`
	TrainerAccountID     uuid.UUID `json:"trainer_account_id"`
	EvaluatorAccountID   uuid.UUID `json:"evaluator_account_id"`
	HeldOutSetSHA256     string    `json:"held_out_set_sha256"`
	ReservedSetSHA256    string    `json:"reserved_set_sha256"`
	AdapterSHA256        string    `json:"adapter_sha256"`
	BaselineModelSHA256  string    `json:"baseline_model_sha256"`
	EvaluationMetric     string    `json:"evaluation_metric"`
	MetricDirection      string    `json:"metric_direction"`
	BaselineScore        float64   `json:"baseline_score"`
	CandidateScore       float64   `json:"candidate_score"`
	RequiredImprovement  float64   `json:"required_improvement"`
	ImprovementFraction  float64   `json:"improvement_fraction"`
	TrainerSawHeldOutSet bool      `json:"trainer_saw_held_out_set"`
	Succeeded            bool      `json:"succeeded"`
	Status               string    `json:"status"`
}

func (r ProjectLoRAEvaluationReceipt) evidence(buyerID uuid.UUID) loraEvaluationEvidence {
	return loraEvaluationEvidence{
		BuyerID: buyerID, CompileReceiptID: r.CompileReceiptID,
		ProjectSHA256: r.ProjectSHA256, IRSHA256: r.IRSHA256, StepID: r.StepID,
		RunID: r.RunID, TrainerWorkerID: r.TrainerWorkerID, EvaluatorWorkerID: r.EvaluatorWorkerID,
		TrainerSupplierID: r.TrainerSupplierID, EvaluatorSupplierID: r.EvaluatorSupplierID,
		TrainerAccountID: r.TrainerAccountID, EvaluatorAccountID: r.EvaluatorAccountID,
		HeldOutSetSHA256: r.HeldOutSetSHA256, ReservedSetSHA256: r.ReservedSetSHA256,
		AdapterSHA256: r.AdapterSHA256, BaselineModelSHA256: r.BaselineModelSHA256,
		EvaluationMetric: r.EvaluationMetric, MetricDirection: r.MetricDirection,
		BaselineScore: r.BaselineScore, CandidateScore: r.CandidateScore,
		RequiredImprovement: r.RequiredImprovement, ImprovementFraction: r.ImprovementFraction,
		TrainerSawHeldOutSet: r.TrainerSawHeldOutSet, Succeeded: r.Succeeded,
		Status: r.Status,
	}
}

func projectLoRAEvaluationEvidenceDigest(buyerID uuid.UUID, receipt ProjectLoRAEvaluationReceipt) (string, error) {
	return canonicalDigest("project LoRA evaluation evidence", receipt.evidence(buyerID))
}

func (s *Store) loadProjectLoRAEvaluationCompileBinding(
	ctx context.Context, compileReceiptID uuid.UUID, stepID string,
) (projectLoRAEvaluationCompileBinding, error) {
	if compileReceiptID == uuid.Nil {
		return projectLoRAEvaluationCompileBinding{}, errProjectLoRAEvaluationReceiptNotFound
	}
	var (
		binding  projectLoRAEvaluationCompileBinding
		status   string
		probed   bool
		approved string
		raw      []byte
	)
	err := s.pool.QueryRow(ctx, `
		SELECT id,buyer_id,project_sha256,ir_sha256,status,probe_requested,
		       COALESCE(approved_ir_sha256,''),ir
		  FROM project_compile_receipts
		 WHERE id=$1`, compileReceiptID).
		Scan(&binding.ID, &binding.BuyerID, &binding.ProjectSHA256, &binding.IRSHA256,
			&status, &probed, &approved, &raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return projectLoRAEvaluationCompileBinding{}, errProjectLoRAEvaluationReceiptNotFound
	}
	if err != nil {
		return projectLoRAEvaluationCompileBinding{}, err
	}
	if err := json.Unmarshal(raw, &binding.IR); err != nil {
		return projectLoRAEvaluationCompileBinding{}, err
	}
	if status != "PROBED_NOT_ADMISSIBLE" || !probed || !binding.IR.Probe.Executed ||
		binding.IR.ProjectSHA256 != binding.ProjectSHA256 || binding.IR.IRSHA256 != binding.IRSHA256 ||
		validateProjectCompileReceiptIR(binding.IR, true) != nil {
		return projectLoRAEvaluationCompileBinding{}, errors.New("LoRA evaluation requires an identity-valid bounded-probe compile receipt")
	}
	if strings.TrimSpace(stepID) == "" || !projectStepIDPattern.MatchString(stepID) {
		return projectLoRAEvaluationCompileBinding{}, errors.New("LoRA evaluation has an invalid step id")
	}
	for _, step := range binding.IR.Steps {
		if step.ID == stepID {
			if step.Kind != "lora_training" || step.LoRA == nil {
				return projectLoRAEvaluationCompileBinding{}, errProjectLoRAEvaluationReceiptNotFound
			}
			binding.Step = step
			return binding, nil
		}
	}
	return projectLoRAEvaluationCompileBinding{}, errProjectLoRAEvaluationReceiptNotFound
}

func (s *Store) lookupProjectLoRAWorkerIdentity(
	ctx context.Context, workerID, expectedSupplierID uuid.UUID,
) (projectLoRAWorkerIdentity, error) {
	if workerID == uuid.Nil {
		return projectLoRAWorkerIdentity{}, errors.New("LoRA worker identity is empty")
	}
	var (
		identity projectLoRAWorkerIdentity
		account  *uuid.UUID
	)
	err := s.pool.QueryRow(ctx, `
		SELECT w.supplier_id,s.owner_buyer_id
		  FROM workers w
		  JOIN suppliers s ON s.id=w.supplier_id
		 WHERE w.id=$1 AND ($2::uuid IS NULL OR w.supplier_id=$2)`, workerID, loraNullableUUID(expectedSupplierID)).
		Scan(&identity.SupplierID, &account)
	if errors.Is(err, pgx.ErrNoRows) {
		return projectLoRAWorkerIdentity{}, errors.New("LoRA worker is not enrolled to a supplier account")
	}
	if err != nil {
		return projectLoRAWorkerIdentity{}, err
	}
	if account == nil || *account == uuid.Nil {
		return projectLoRAWorkerIdentity{}, errors.New("LoRA worker supplier has no buyer account owner")
	}
	identity.WorkerID, identity.AccountID = workerID, *account
	return identity, nil
}

// nullUUID keeps the worker identity query explicit: an authenticated token's
// supplier binding is required when expectedSupplierID is non-zero, while the
// trainer lookup can use the worker id alone.
func loraNullableUUID(id uuid.UUID) *uuid.UUID {
	if id == uuid.Nil {
		return nil
	}
	return &id
}

func validateProjectLoRAEvaluationEvidence(
	eval loraEvaluation, metricDirection string,
) (float64, bool, error) {
	if err := validateLoRAEvaluation(eval); err != nil {
		return 0, false, err
	}
	if metricDirection != "HIGHER_IS_BETTER" && metricDirection != "LOWER_IS_BETTER" {
		return 0, false, errors.New("LoRA evaluation has an unsupported metric direction")
	}
	if eval.CandidateScore < 0 {
		return 0, false, fmt.Errorf("%w: candidate score must be non-negative", errLoRAEvaluationShape)
	}
	var improvement float64
	if metricDirection == "LOWER_IS_BETTER" {
		improvement = (eval.BaselineScore - eval.CandidateScore) / eval.BaselineScore
	} else {
		improvement = (eval.CandidateScore - eval.BaselineScore) / eval.BaselineScore
	}
	if !moneyUSDInDomain(improvement) {
		return 0, false, fmt.Errorf("%w: score improvement is not finite", errLoRAEvaluationShape)
	}
	return improvement, improvement >= eval.RequiredImprovement, nil
}

func validateProjectLoRAEvaluationReceipt(
	receipt ProjectLoRAEvaluationReceipt, buyerID uuid.UUID, binding projectLoRAEvaluationCompileBinding,
) error {
	if receipt.Version != projectLoRAEvaluationReceiptVersion ||
		receipt.Status != projectLoRAEvaluationReceiptStatus ||
		receipt.ID == uuid.Nil || receipt.CompileReceiptID != binding.ID ||
		receipt.ProjectSHA256 != binding.ProjectSHA256 || receipt.IRSHA256 != binding.IRSHA256 ||
		receipt.StepID != binding.Step.ID || receipt.RunID == uuid.Nil ||
		receipt.TrainerWorkerID == uuid.Nil || receipt.EvaluatorWorkerID == uuid.Nil ||
		receipt.TrainerWorkerID == receipt.EvaluatorWorkerID ||
		receipt.TrainerSupplierID == uuid.Nil || receipt.EvaluatorSupplierID == uuid.Nil ||
		receipt.TrainerAccountID == uuid.Nil || receipt.EvaluatorAccountID == uuid.Nil ||
		receipt.HeldOutSetSHA256 != receipt.ReservedSetSHA256 ||
		receipt.HeldOutSetSHA256 != strings.ToLower(binding.Step.LoRA.HeldOutSet.SHA256) ||
		!validSHA256(receipt.HeldOutSetSHA256) || !validSHA256(receipt.AdapterSHA256) ||
		!validSHA256(receipt.BaselineModelSHA256) || receipt.EvaluationMetric != binding.Step.LoRA.EvaluationMetric ||
		receipt.MetricDirection != binding.Step.LoRA.MetricDirection ||
		receipt.RequiredImprovement != binding.Step.LoRA.RequiredImprovement {
		return errors.New("stored LoRA evaluation receipt authority drifted")
	}
	eval := loraEvaluation{
		RunID: receipt.RunID, TrainerSupplierID: receipt.TrainerSupplierID,
		EvaluatorSupplierID: receipt.EvaluatorSupplierID, TrainerAccountID: receipt.TrainerAccountID,
		EvaluatorAccountID: receipt.EvaluatorAccountID, HeldOutSetSHA256: receipt.HeldOutSetSHA256,
		ReservedSetSHA256: receipt.ReservedSetSHA256, TrainerSawHeldOutSet: receipt.TrainerSawHeldOutSet,
		BaselineScore: receipt.BaselineScore, CandidateScore: receipt.CandidateScore,
		RequiredImprovement: receipt.RequiredImprovement,
	}
	improvement, succeeded, err := validateProjectLoRAEvaluationEvidence(eval, receipt.MetricDirection)
	if err != nil || math.Abs(improvement-receipt.ImprovementFraction) > 1e-12 || succeeded != receipt.Succeeded {
		if err != nil {
			return fmt.Errorf("stored LoRA evaluation receipt failed validation: %w", err)
		}
		return errors.New("stored LoRA evaluation outcome drifted")
	}
	digest, err := projectLoRAEvaluationEvidenceDigest(buyerID, receipt)
	if err != nil || digest != receipt.EvidenceSHA256 {
		return errors.New("stored LoRA evaluation evidence digest drifted")
	}
	return nil
}

const projectLoRAEvaluationReceiptColumns = `
	id,buyer_id,compile_receipt_id,project_sha256,ir_sha256,step_id,run_id,
	trainer_worker_id,evaluator_worker_id,trainer_supplier_id,evaluator_supplier_id,
	trainer_account_id,evaluator_account_id,held_out_set_sha256,reserved_set_sha256,
	adapter_sha256,baseline_model_sha256,evaluation_metric,metric_direction,
	baseline_score,candidate_score,required_improvement,improvement_fraction,
	trainer_saw_held_out_set,succeeded,status,evidence_sha256,evidence,created_at`

// RecordProjectLoRAEvaluation appends one evaluator observation. The evaluator
// worker and both supplier account owners are derived server-side, and the
// compile receipt supplies the held-out pin, model identity, metric, and margin.
func (s *Store) RecordProjectLoRAEvaluation(
	ctx context.Context, auth WorkerAuth, submission ProjectLoRAEvaluationSubmission,
) (ProjectLoRAEvaluationReceipt, error) {
	if auth.WorkerID == uuid.Nil || auth.SupplierID == uuid.Nil {
		return ProjectLoRAEvaluationReceipt{}, errors.New("LoRA evaluation requires an authenticated worker")
	}
	submission.StepID = strings.TrimSpace(submission.StepID)
	submission.AdapterSHA256 = strings.ToLower(strings.TrimSpace(submission.AdapterSHA256))
	submission.HeldOutSetSHA256 = strings.ToLower(strings.TrimSpace(submission.HeldOutSetSHA256))
	if submission.CompileReceiptID == uuid.Nil || submission.RunID == uuid.Nil ||
		submission.TrainerWorkerID == uuid.Nil || !projectStepIDPattern.MatchString(submission.StepID) ||
		!validSHA256(submission.AdapterSHA256) || !validSHA256(submission.HeldOutSetSHA256) {
		return ProjectLoRAEvaluationReceipt{}, errors.New("LoRA evaluation submission has incomplete identities")
	}
	binding, err := s.loadProjectLoRAEvaluationCompileBinding(ctx, submission.CompileReceiptID, submission.StepID)
	if err != nil {
		return ProjectLoRAEvaluationReceipt{}, err
	}
	evaluator, err := s.lookupProjectLoRAWorkerIdentity(ctx, auth.WorkerID, auth.SupplierID)
	if err != nil {
		return ProjectLoRAEvaluationReceipt{}, err
	}
	trainer, err := s.lookupProjectLoRAWorkerIdentity(ctx, submission.TrainerWorkerID, uuid.Nil)
	if err != nil {
		return ProjectLoRAEvaluationReceipt{}, err
	}
	if evaluator.WorkerID == trainer.WorkerID {
		return ProjectLoRAEvaluationReceipt{}, fmt.Errorf("%w: evaluator and trainer worker are the same", errLoRANotIndependent)
	}
	contract := binding.Step.LoRA
	eval := loraEvaluation{
		RunID: submission.RunID, TrainerSupplierID: trainer.SupplierID,
		EvaluatorSupplierID: evaluator.SupplierID, TrainerAccountID: trainer.AccountID,
		EvaluatorAccountID: evaluator.AccountID, HeldOutSetSHA256: submission.HeldOutSetSHA256,
		ReservedSetSHA256:    strings.ToLower(contract.HeldOutSet.SHA256),
		TrainerSawHeldOutSet: submission.TrainerSawHeldOutSet, BaselineScore: submission.BaselineScore,
		CandidateScore: submission.CandidateScore, RequiredImprovement: contract.RequiredImprovement,
	}
	improvement, succeeded, err := validateProjectLoRAEvaluationEvidence(eval, contract.MetricDirection)
	if err != nil {
		return ProjectLoRAEvaluationReceipt{}, err
	}
	receipt := ProjectLoRAEvaluationReceipt{
		ID: uuid.New(), CompileReceiptID: binding.ID, ProjectSHA256: binding.ProjectSHA256,
		IRSHA256: binding.IRSHA256, StepID: binding.Step.ID, RunID: submission.RunID,
		TrainerWorkerID: trainer.WorkerID, EvaluatorWorkerID: evaluator.WorkerID,
		TrainerSupplierID: trainer.SupplierID, EvaluatorSupplierID: evaluator.SupplierID,
		TrainerAccountID: trainer.AccountID, EvaluatorAccountID: evaluator.AccountID,
		HeldOutSetSHA256: submission.HeldOutSetSHA256, ReservedSetSHA256: strings.ToLower(contract.HeldOutSet.SHA256),
		AdapterSHA256: submission.AdapterSHA256, BaselineModelSHA256: strings.ToLower(contract.BaselineModelSHA256),
		EvaluationMetric: contract.EvaluationMetric, MetricDirection: contract.MetricDirection,
		BaselineScore: submission.BaselineScore, CandidateScore: submission.CandidateScore,
		RequiredImprovement: contract.RequiredImprovement, ImprovementFraction: improvement,
		TrainerSawHeldOutSet: submission.TrainerSawHeldOutSet, Succeeded: succeeded,
		Status: projectLoRAEvaluationReceiptStatus,
	}
	receipt.EvidenceSHA256, err = projectLoRAEvaluationEvidenceDigest(binding.BuyerID, receipt)
	if err != nil {
		return ProjectLoRAEvaluationReceipt{}, err
	}
	expectedEvidenceSHA := receipt.EvidenceSHA256
	evidenceRaw, err := json.Marshal(receipt.evidence(binding.BuyerID))
	if err != nil {
		return ProjectLoRAEvaluationReceipt{}, err
	}
	var storedBuyer uuid.UUID
	err = s.pool.QueryRow(ctx, `
		INSERT INTO project_lora_evaluation_receipts
		  (id,buyer_id,compile_receipt_id,project_sha256,ir_sha256,step_id,run_id,
		   trainer_worker_id,evaluator_worker_id,trainer_supplier_id,evaluator_supplier_id,
		   trainer_account_id,evaluator_account_id,held_out_set_sha256,reserved_set_sha256,
		   adapter_sha256,baseline_model_sha256,evaluation_metric,metric_direction,
		   baseline_score,candidate_score,required_improvement,improvement_fraction,
		   trainer_saw_held_out_set,succeeded,status,evidence_sha256,evidence)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,
		        $20,$21,$22,$23,$24,$25,$26,$27,$28)
		ON CONFLICT (compile_receipt_id,step_id,evaluator_worker_id,run_id) DO NOTHING
		RETURNING `+projectLoRAEvaluationReceiptColumns,
		receipt.ID, binding.BuyerID, receipt.CompileReceiptID, receipt.ProjectSHA256, receipt.IRSHA256,
		receipt.StepID, receipt.RunID, receipt.TrainerWorkerID, receipt.EvaluatorWorkerID,
		receipt.TrainerSupplierID, receipt.EvaluatorSupplierID, receipt.TrainerAccountID,
		receipt.EvaluatorAccountID, receipt.HeldOutSetSHA256, receipt.ReservedSetSHA256,
		receipt.AdapterSHA256, receipt.BaselineModelSHA256, receipt.EvaluationMetric,
		receipt.MetricDirection, receipt.BaselineScore, receipt.CandidateScore,
		receipt.RequiredImprovement, receipt.ImprovementFraction, receipt.TrainerSawHeldOutSet,
		receipt.Succeeded, receipt.Status, receipt.EvidenceSHA256, evidenceRaw).
		Scan(&receipt.ID, &storedBuyer, &receipt.CompileReceiptID, &receipt.ProjectSHA256, &receipt.IRSHA256,
			&receipt.StepID, &receipt.RunID, &receipt.TrainerWorkerID, &receipt.EvaluatorWorkerID,
			&receipt.TrainerSupplierID, &receipt.EvaluatorSupplierID, &receipt.TrainerAccountID,
			&receipt.EvaluatorAccountID, &receipt.HeldOutSetSHA256, &receipt.ReservedSetSHA256,
			&receipt.AdapterSHA256, &receipt.BaselineModelSHA256, &receipt.EvaluationMetric,
			&receipt.MetricDirection, &receipt.BaselineScore, &receipt.CandidateScore,
			&receipt.RequiredImprovement, &receipt.ImprovementFraction, &receipt.TrainerSawHeldOutSet,
			&receipt.Succeeded, &receipt.Status, &receipt.EvidenceSHA256, &evidenceRaw, &receipt.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		err = s.pool.QueryRow(ctx, `SELECT `+projectLoRAEvaluationReceiptColumns+`
			FROM project_lora_evaluation_receipts
			WHERE compile_receipt_id=$1 AND step_id=$2 AND evaluator_worker_id=$3 AND run_id=$4`,
			submission.CompileReceiptID, submission.StepID, evaluator.WorkerID, submission.RunID).
			Scan(&receipt.ID, &storedBuyer, &receipt.CompileReceiptID, &receipt.ProjectSHA256, &receipt.IRSHA256,
				&receipt.StepID, &receipt.RunID, &receipt.TrainerWorkerID, &receipt.EvaluatorWorkerID,
				&receipt.TrainerSupplierID, &receipt.EvaluatorSupplierID, &receipt.TrainerAccountID,
				&receipt.EvaluatorAccountID, &receipt.HeldOutSetSHA256, &receipt.ReservedSetSHA256,
				&receipt.AdapterSHA256, &receipt.BaselineModelSHA256, &receipt.EvaluationMetric,
				&receipt.MetricDirection, &receipt.BaselineScore, &receipt.CandidateScore,
				&receipt.RequiredImprovement, &receipt.ImprovementFraction, &receipt.TrainerSawHeldOutSet,
				&receipt.Succeeded, &receipt.Status, &receipt.EvidenceSHA256, &evidenceRaw, &receipt.CreatedAt)
		if err == nil && receipt.EvidenceSHA256 != expectedEvidenceSHA {
			return ProjectLoRAEvaluationReceipt{}, errors.New("LoRA run identity was already recorded with different evidence")
		}
	}
	if err != nil {
		return ProjectLoRAEvaluationReceipt{}, err
	}
	receipt.Version = projectLoRAEvaluationReceiptVersion
	if storedBuyer != binding.BuyerID || receipt.EvidenceSHA256 != projectLoRAEvaluationReceiptStatusDigest(binding.BuyerID, receipt) {
		return ProjectLoRAEvaluationReceipt{}, errors.New("stored LoRA evaluation receipt identity drifted")
	}
	if err := validateProjectLoRAEvaluationReceipt(receipt, binding.BuyerID, binding); err != nil {
		return ProjectLoRAEvaluationReceipt{}, err
	}
	receipt.ExecutionRefusal = projectLoRAEvaluationExecutionRefusal
	return receipt, nil
}

// projectLoRAEvaluationReceiptStatusDigest is intentionally a small helper so
// the idempotent insert and replay paths cannot accidentally compare a
// different digest implementation.
func projectLoRAEvaluationReceiptStatusDigest(buyerID uuid.UUID, receipt ProjectLoRAEvaluationReceipt) string {
	digest, _ := projectLoRAEvaluationEvidenceDigest(buyerID, receipt)
	return digest
}

func (s *Store) GetProjectLoRAEvaluationReceipt(
	ctx context.Context, buyerID, receiptID uuid.UUID,
) (ProjectLoRAEvaluationReceipt, error) {
	if buyerID == uuid.Nil || receiptID == uuid.Nil {
		return ProjectLoRAEvaluationReceipt{}, errProjectLoRAEvaluationReceiptNotFound
	}
	var (
		receipt     ProjectLoRAEvaluationReceipt
		storedBuyer uuid.UUID
		evidence    []byte
	)
	err := s.pool.QueryRow(ctx, `SELECT `+projectLoRAEvaluationReceiptColumns+`
		FROM project_lora_evaluation_receipts WHERE id=$1 AND buyer_id=$2`, receiptID, buyerID).
		Scan(&receipt.ID, &storedBuyer, &receipt.CompileReceiptID, &receipt.ProjectSHA256, &receipt.IRSHA256,
			&receipt.StepID, &receipt.RunID, &receipt.TrainerWorkerID, &receipt.EvaluatorWorkerID,
			&receipt.TrainerSupplierID, &receipt.EvaluatorSupplierID, &receipt.TrainerAccountID,
			&receipt.EvaluatorAccountID, &receipt.HeldOutSetSHA256, &receipt.ReservedSetSHA256,
			&receipt.AdapterSHA256, &receipt.BaselineModelSHA256, &receipt.EvaluationMetric,
			&receipt.MetricDirection, &receipt.BaselineScore, &receipt.CandidateScore,
			&receipt.RequiredImprovement, &receipt.ImprovementFraction, &receipt.TrainerSawHeldOutSet,
			&receipt.Succeeded, &receipt.Status, &receipt.EvidenceSHA256, &evidence, &receipt.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return ProjectLoRAEvaluationReceipt{}, errProjectLoRAEvaluationReceiptNotFound
	}
	if err != nil {
		return ProjectLoRAEvaluationReceipt{}, err
	}
	binding, err := s.loadProjectLoRAEvaluationCompileBinding(ctx, receipt.CompileReceiptID, receipt.StepID)
	if errors.Is(err, errProjectLoRAEvaluationReceiptNotFound) {
		return ProjectLoRAEvaluationReceipt{}, errProjectLoRAEvaluationReceiptNotFound
	}
	if err != nil {
		return ProjectLoRAEvaluationReceipt{}, err
	}
	if storedBuyer != buyerID || string(evidence) == "" ||
		receipt.EvidenceSHA256 != projectLoRAEvaluationReceiptStatusDigest(buyerID, receipt) {
		return ProjectLoRAEvaluationReceipt{}, errors.New("stored LoRA evaluation receipt identity drifted")
	}
	receipt.Version = projectLoRAEvaluationReceiptVersion
	if err := validateProjectLoRAEvaluationReceipt(receipt, buyerID, binding); err != nil {
		return ProjectLoRAEvaluationReceipt{}, err
	}
	receipt.ExecutionRefusal = projectLoRAEvaluationExecutionRefusal
	return receipt, nil
}
