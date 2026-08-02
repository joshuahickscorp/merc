package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

var errProjectRenderAssemblyReceiptNotFound = errors.New("project render assembly receipt not found")

func renderAssemblyManifestDigest(manifest ProjectRenderAssemblyManifest) (string, error) {
	if manifest.Units == nil {
		return "", errors.New("render assembly manifest units are required")
	}
	return canonicalDigest("project render assembly manifest", manifest)
}

// RecordProjectRenderAssemblyReceipt persists a complete, buyer-scoped
// assembly observation against a durable probed compile receipt. It never
// writes an execution contract, transfers an asset, or creates a ledger row.
func (s *Store) RecordProjectRenderAssemblyReceipt(
	ctx context.Context, buyerID, compileReceiptID uuid.UUID, stepID string,
	manifest ProjectRenderAssemblyManifest,
) (ProjectRenderAssemblyReceipt, error) {
	if buyerID == uuid.Nil || compileReceiptID == uuid.Nil {
		return ProjectRenderAssemblyReceipt{}, errors.New("render assembly receipt requires buyer and compile receipt")
	}
	stepID = strings.TrimSpace(stepID)
	if !projectStepIDPattern.MatchString(stepID) {
		return ProjectRenderAssemblyReceipt{}, errors.New("render assembly receipt has an invalid step id")
	}
	compile, err := s.GetProjectCompileReceipt(ctx, buyerID, compileReceiptID)
	if errors.Is(err, errProjectCompileReceiptNotFound) {
		return ProjectRenderAssemblyReceipt{}, errProjectRenderAssemblyReceiptNotFound
	}
	if err != nil {
		return ProjectRenderAssemblyReceipt{}, err
	}
	if !compile.IR.Probe.Executed || compile.Status != "PROBED_NOT_ADMISSIBLE" {
		return ProjectRenderAssemblyReceipt{}, errors.New("render assembly requires a durable bounded-probe compile receipt")
	}
	var step *ProjectIRStep
	for i := range compile.IR.Steps {
		if compile.IR.Steps[i].ID == stepID {
			step = &compile.IR.Steps[i]
			break
		}
	}
	if step == nil || step.Kind != "media_rendering" || step.Rendering == nil {
		return ProjectRenderAssemblyReceipt{}, errProjectRenderAssemblyReceiptNotFound
	}
	summary, err := validateProjectRenderAssemblyManifest(*step.Rendering, manifest)
	if err != nil {
		return ProjectRenderAssemblyReceipt{}, fmt.Errorf("render assembly manifest refused: %w", err)
	}
	digest, err := renderAssemblyManifestDigest(manifest)
	if err != nil {
		return ProjectRenderAssemblyReceipt{}, err
	}
	raw, err := json.Marshal(manifest)
	if err != nil {
		return ProjectRenderAssemblyReceipt{}, err
	}
	const status = "ASSEMBLY_MANIFEST_VERIFIED_NOT_EXECUTABLE"
	const refusal = "render assembly remains non-executable: no governed renderer, asset locality, output verifier, worker assignment, or settlement authority is present"
	id := uuid.New()
	var out ProjectRenderAssemblyReceipt
	var stored []byte
	err = s.pool.QueryRow(ctx, `
		INSERT INTO project_render_assembly_receipts
		  (id,buyer_id,compile_receipt_id,project_sha256,ir_sha256,step_id,
		   manifest_sha256,status,unit_count,succeeded_units,failed_attempts,
		   replaced_ordinals,manifest)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)
		ON CONFLICT (compile_receipt_id,step_id,manifest_sha256) DO NOTHING
		RETURNING id,compile_receipt_id,project_sha256,ir_sha256,step_id,
		          manifest_sha256,status,unit_count,succeeded_units,failed_attempts,
		          replaced_ordinals,manifest,created_at`,
		id, buyerID, compileReceiptID, compile.ProjectSHA256, compile.IRSHA256,
		stepID, digest, status, summary.UnitCount, summary.SucceededUnits,
		summary.FailedAttempts, summary.ReplacedOrdinals, raw).
		Scan(&out.ID, &out.CompileReceiptID, &out.ProjectSHA256, &out.IRSHA256,
			&out.StepID, &out.ManifestSHA256, &out.Status, &out.UnitCount,
			&out.SucceededUnits, &out.FailedAttempts, &out.ReplacedOrdinals,
			&stored, &out.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		err = s.pool.QueryRow(ctx, `
			SELECT id,compile_receipt_id,project_sha256,ir_sha256,step_id,
			       manifest_sha256,status,unit_count,succeeded_units,failed_attempts,
			       replaced_ordinals,manifest,created_at
			  FROM project_render_assembly_receipts
			 WHERE buyer_id=$1 AND compile_receipt_id=$2 AND step_id=$3 AND manifest_sha256=$4`,
			buyerID, compileReceiptID, stepID, digest).
			Scan(&out.ID, &out.CompileReceiptID, &out.ProjectSHA256, &out.IRSHA256,
				&out.StepID, &out.ManifestSHA256, &out.Status, &out.UnitCount,
				&out.SucceededUnits, &out.FailedAttempts, &out.ReplacedOrdinals,
				&stored, &out.CreatedAt)
	}
	if err != nil {
		return ProjectRenderAssemblyReceipt{}, err
	}
	if out.CompileReceiptID != compileReceiptID.String() || out.ProjectSHA256 != compile.ProjectSHA256 ||
		out.IRSHA256 != compile.IRSHA256 || out.ManifestSHA256 != digest ||
		out.Status != status || out.UnitCount != summary.UnitCount ||
		out.SucceededUnits != summary.SucceededUnits || out.FailedAttempts != summary.FailedAttempts ||
		out.ReplacedOrdinals != summary.ReplacedOrdinals {
		return ProjectRenderAssemblyReceipt{}, errors.New("stored render assembly receipt authority drifted")
	}
	var decoded ProjectRenderAssemblyManifest
	if err := json.Unmarshal(stored, &decoded); err != nil {
		return ProjectRenderAssemblyReceipt{}, err
	}
	if _, err := validateProjectRenderAssemblyManifest(*step.Rendering, decoded); err != nil {
		return ProjectRenderAssemblyReceipt{}, fmt.Errorf("stored render assembly receipt failed validation: %w", err)
	}
	out.Version = renderAssemblyReceiptVersion
	out.Units = decoded.Units
	out.ExecutionRefusal = refusal
	return out, nil
}

func (s *Store) GetProjectRenderAssemblyReceipt(
	ctx context.Context, buyerID, receiptID uuid.UUID,
) (ProjectRenderAssemblyReceipt, error) {
	if buyerID == uuid.Nil || receiptID == uuid.Nil {
		return ProjectRenderAssemblyReceipt{}, errProjectRenderAssemblyReceiptNotFound
	}
	var out ProjectRenderAssemblyReceipt
	var compileID uuid.UUID
	var raw []byte
	err := s.pool.QueryRow(ctx, `
		SELECT id,compile_receipt_id,project_sha256,ir_sha256,step_id,
		       manifest_sha256,status,unit_count,succeeded_units,failed_attempts,
		       replaced_ordinals,manifest,created_at
		  FROM project_render_assembly_receipts
		 WHERE id=$1 AND buyer_id=$2`, receiptID, buyerID).
		Scan(&out.ID, &compileID, &out.ProjectSHA256, &out.IRSHA256, &out.StepID,
			&out.ManifestSHA256, &out.Status, &out.UnitCount, &out.SucceededUnits,
			&out.FailedAttempts, &out.ReplacedOrdinals, &raw, &out.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return ProjectRenderAssemblyReceipt{}, errProjectRenderAssemblyReceiptNotFound
	}
	if err != nil {
		return ProjectRenderAssemblyReceipt{}, err
	}
	out.CompileReceiptID = compileID.String()
	var manifest ProjectRenderAssemblyManifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return ProjectRenderAssemblyReceipt{}, err
	}
	compile, err := s.GetProjectCompileReceipt(ctx, buyerID, compileID)
	if errors.Is(err, errProjectCompileReceiptNotFound) {
		return ProjectRenderAssemblyReceipt{}, errProjectRenderAssemblyReceiptNotFound
	}
	if err != nil {
		return ProjectRenderAssemblyReceipt{}, err
	}
	var step *ProjectIRStep
	for i := range compile.IR.Steps {
		if compile.IR.Steps[i].ID == out.StepID {
			step = &compile.IR.Steps[i]
			break
		}
	}
	if step == nil || step.Rendering == nil || step.Kind != "media_rendering" {
		return ProjectRenderAssemblyReceipt{}, errors.New("stored render assembly step is not a rendering step")
	}
	summary, err := validateProjectRenderAssemblyManifest(*step.Rendering, manifest)
	if err != nil {
		return ProjectRenderAssemblyReceipt{}, fmt.Errorf("stored render assembly receipt failed validation: %w", err)
	}
	digest, err := renderAssemblyManifestDigest(manifest)
	if err != nil || digest != out.ManifestSHA256 || out.CompileReceiptID != compileID.String() ||
		out.ProjectSHA256 != compile.ProjectSHA256 || out.IRSHA256 != compile.IRSHA256 ||
		out.UnitCount != summary.UnitCount || out.SucceededUnits != summary.SucceededUnits ||
		out.FailedAttempts != summary.FailedAttempts || out.ReplacedOrdinals != summary.ReplacedOrdinals {
		return ProjectRenderAssemblyReceipt{}, errors.New("stored render assembly receipt identity drifted")
	}
	out.Version = renderAssemblyReceiptVersion
	out.Units = manifest.Units
	out.ExecutionRefusal = "render assembly remains non-executable: no governed renderer, asset locality, output verifier, worker assignment, or settlement authority is present"
	return out, nil
}
