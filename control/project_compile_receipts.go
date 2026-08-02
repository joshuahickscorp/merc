package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ProjectCompileReceipt is durable, buyer-scoped evidence for the compiler
// proposal or bounded non-executing probe. It is deliberately not a quote,
// capacity reservation, or execution contract.
type ProjectCompileReceipt struct {
	ID               string            `json:"id"`
	ProjectSHA256    string            `json:"project_sha256"`
	IRSHA256         string            `json:"ir_sha256"`
	Status           string            `json:"status"`
	ProbeRequested   bool              `json:"probe_requested"`
	ApprovedIRSHA256 string            `json:"approved_ir_sha256,omitempty"`
	IR               ProjectWorkloadIR `json:"ir"`
	CreatedAt        time.Time         `json:"created_at"`
}

func projectIRProjectDigest(ir ProjectWorkloadIR) (string, error) {
	artifacts := append([]ProjectIRArtifact(nil), ir.Artifacts...)
	sort.Slice(artifacts, func(i, j int) bool { return artifacts[i].Path < artifacts[j].Path })
	hash := sha256.New()
	for _, artifact := range artifacts {
		if strings.TrimSpace(artifact.Path) == "" || !validSHA256(artifact.SHA256) || artifact.Bytes < 0 {
			return "", errors.New("project IR contains an invalid artifact identity")
		}
		fmt.Fprintf(hash, "%s\x00%d\x00%s\n", artifact.Path, artifact.Bytes, artifact.SHA256)
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func validateProjectCompileReceiptIR(ir ProjectWorkloadIR, probeRequested bool) error {
	if ir.Version != projectIRVersion || !validSHA256(ir.ProjectSHA256) || !validSHA256(ir.IRSHA256) {
		return errors.New("project compile receipt has invalid IR identity")
	}
	projectDigest, err := projectIRProjectDigest(ir)
	if err != nil {
		return err
	}
	if projectDigest != ir.ProjectSHA256 {
		return errors.New("project compile receipt project digest does not match its artifact inventory")
	}
	irDigest, err := projectIRDigest(ir)
	if err != nil {
		return err
	}
	if irDigest != ir.IRSHA256 {
		return errors.New("project compile receipt IR digest does not match its graph")
	}
	if ir.Probe.Requested != probeRequested || ir.Probe.Executed != probeRequested {
		return errors.New("project compile receipt probe state does not match the request")
	}
	if probeRequested {
		if !validSHA256(ir.Probe.ApprovedIRSHA256) {
			return errors.New("probed project compile receipt lacks the approved proposal digest")
		}
	} else if ir.Probe.ApprovedIRSHA256 != "" {
		return errors.New("proposal project compile receipt must not name an approved probe digest")
	}
	return nil
}

func (s *Store) RecordProjectCompileReceipt(
	ctx context.Context, buyerID uuid.UUID, ir ProjectWorkloadIR, probeRequested bool,
) (ProjectCompileReceipt, error) {
	if buyerID == uuid.Nil {
		return ProjectCompileReceipt{}, errors.New("project compile receipt requires a buyer")
	}
	if err := validateProjectCompileReceiptIR(ir, probeRequested); err != nil {
		return ProjectCompileReceipt{}, err
	}
	approved := strings.TrimSpace(ir.Probe.ApprovedIRSHA256)
	status := "PROPOSED_NOT_ADMISSIBLE"
	if probeRequested {
		status = "PROBED_NOT_ADMISSIBLE"
		var exists bool
		if err := s.pool.QueryRow(ctx, `SELECT EXISTS(
			SELECT 1 FROM project_compile_receipts
			 WHERE buyer_id=$1 AND project_sha256=$2 AND ir_sha256=$3
			   AND status='PROPOSED_NOT_ADMISSIBLE' AND probe_requested=false)`,
			buyerID, ir.ProjectSHA256, approved).Scan(&exists); err != nil {
			return ProjectCompileReceipt{}, err
		}
		if !exists {
			return ProjectCompileReceipt{}, errors.New("bounded probe must follow a durable buyer-scoped proposal receipt")
		}
	}
	raw, err := json.Marshal(ir)
	if err != nil {
		return ProjectCompileReceipt{}, err
	}
	id := uuid.New()
	var out ProjectCompileReceipt
	err = s.pool.QueryRow(ctx, `
		INSERT INTO project_compile_receipts
		  (id,buyer_id,project_sha256,ir_sha256,status,probe_requested,approved_ir_sha256,ir)
		VALUES ($1,$2,$3,$4,$5,$6,NULLIF($7,''),$8)
		ON CONFLICT (buyer_id,project_sha256,ir_sha256) DO NOTHING
		RETURNING id,project_sha256,ir_sha256,status,probe_requested,
		          COALESCE(approved_ir_sha256,''),ir,created_at`,
		id, buyerID, ir.ProjectSHA256, ir.IRSHA256, status, probeRequested, approved, raw).
		Scan(&out.ID, &out.ProjectSHA256, &out.IRSHA256, &out.Status, &out.ProbeRequested,
			&out.ApprovedIRSHA256, &raw, &out.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		err = s.pool.QueryRow(ctx, `
			SELECT id,project_sha256,ir_sha256,status,probe_requested,
			       COALESCE(approved_ir_sha256,''),ir,created_at
			  FROM project_compile_receipts
			 WHERE buyer_id=$1 AND project_sha256=$2 AND ir_sha256=$3`,
			buyerID, ir.ProjectSHA256, ir.IRSHA256).
			Scan(&out.ID, &out.ProjectSHA256, &out.IRSHA256, &out.Status, &out.ProbeRequested,
				&out.ApprovedIRSHA256, &raw, &out.CreatedAt)
	}
	if err != nil {
		return ProjectCompileReceipt{}, err
	}
	if err := json.Unmarshal(raw, &out.IR); err != nil {
		return ProjectCompileReceipt{}, err
	}
	if out.IR.ProjectSHA256 != out.ProjectSHA256 || out.IR.IRSHA256 != out.IRSHA256 {
		return ProjectCompileReceipt{}, errors.New("stored project compile receipt identity drifted")
	}
	return out, nil
}

func (s *Store) GetProjectCompileReceipt(ctx context.Context, buyerID, receiptID uuid.UUID) (ProjectCompileReceipt, error) {
	if buyerID == uuid.Nil || receiptID == uuid.Nil {
		return ProjectCompileReceipt{}, errProjectCompileReceiptNotFound
	}
	var out ProjectCompileReceipt
	var raw []byte
	err := s.pool.QueryRow(ctx, `
		SELECT id,project_sha256,ir_sha256,status,probe_requested,
		       COALESCE(approved_ir_sha256,''),ir,created_at
		  FROM project_compile_receipts
		 WHERE id=$1 AND buyer_id=$2`, receiptID, buyerID).
		Scan(&out.ID, &out.ProjectSHA256, &out.IRSHA256, &out.Status, &out.ProbeRequested,
			&out.ApprovedIRSHA256, &raw, &out.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return ProjectCompileReceipt{}, errProjectCompileReceiptNotFound
	}
	if err != nil {
		return ProjectCompileReceipt{}, err
	}
	if err := json.Unmarshal(raw, &out.IR); err != nil {
		return ProjectCompileReceipt{}, err
	}
	if out.IR.ProjectSHA256 != out.ProjectSHA256 || out.IR.IRSHA256 != out.IRSHA256 ||
		validateProjectCompileReceiptIR(out.IR, out.ProbeRequested) != nil {
		return ProjectCompileReceipt{}, errors.New("stored project compile receipt failed identity validation")
	}
	return out, nil
}

var errProjectCompileReceiptNotFound = errors.New("project compile receipt not found")
