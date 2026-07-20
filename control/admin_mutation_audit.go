package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const adminMutationIntentVersion = 1

const (
	adminActionWorkerSuspended   = "worker_suspended"
	adminActionWorkerReinstated  = "worker_reinstated"
	adminActionTaskRequeued      = "task_requeued"
	adminActionReputationChanged = "reputation_adjusted"
	adminActionPayoutReleased    = "payout_released"
	adminActionControlChanged    = "operational_control_changed"

	adminTargetWorker      = "worker"
	adminTargetTask        = "task"
	adminTargetSupplier    = "supplier"
	adminTargetLedgerEntry = "ledger_entry"
	adminTargetControl     = "operational_control"
)

var errAdminMutationInvalid = errors.New("invalid admin mutation")

type adminMutationIntent struct {
	Version        int       `json:"version"`
	Kind           string    `json:"kind"`
	TargetKind     string    `json:"target_kind"`
	TargetID       uuid.UUID `json:"target_id"`
	Reason         string    `json:"reason"`
	CorrelationRef string    `json:"correlation_ref,omitempty"`
	Delta          *float32  `json:"delta,omitempty"`
}

func (in adminMutationIntent) normalized() adminMutationIntent {
	in.Version = adminMutationIntentVersion
	in.Kind = strings.TrimSpace(in.Kind)
	in.TargetKind = strings.TrimSpace(in.TargetKind)
	in.Reason = strings.TrimSpace(in.Reason)
	in.CorrelationRef = strings.TrimSpace(in.CorrelationRef)
	return in
}

func (in adminMutationIntent) validate() error {
	if in.Version != adminMutationIntentVersion || in.TargetID == uuid.Nil {
		return fmt.Errorf("%w: version and target are required", errAdminMutationInvalid)
	}
	if in.Reason == "" {
		return fmt.Errorf("%w: reason is required", errAdminMutationInvalid)
	}
	if len(in.Reason) > 1000 || len(in.CorrelationRef) > 200 {
		return fmt.Errorf("%w: reason or request_id is too long", errAdminMutationInvalid)
	}
	wantTarget := ""
	switch in.Kind {
	case adminActionWorkerSuspended, adminActionWorkerReinstated:
		wantTarget = adminTargetWorker
	case adminActionTaskRequeued:
		wantTarget = adminTargetTask
	case adminActionReputationChanged:
		wantTarget = adminTargetSupplier
		if in.Delta == nil || *in.Delta == 0 || math.IsNaN(float64(*in.Delta)) || math.IsInf(float64(*in.Delta), 0) {
			return fmt.Errorf("%w: reputation delta must be finite and non-zero", errAdminMutationInvalid)
		}
	case adminActionPayoutReleased:
		wantTarget = adminTargetLedgerEntry
	case adminActionControlChanged:
		wantTarget = adminTargetControl
	default:
		return fmt.Errorf("%w: unsupported action %q", errAdminMutationInvalid, in.Kind)
	}
	if in.TargetKind != wantTarget {
		return fmt.Errorf("%w: action %q requires target kind %q", errAdminMutationInvalid, in.Kind, wantTarget)
	}
	if in.Kind != adminActionReputationChanged && in.Delta != nil {
		return fmt.Errorf("%w: action %q does not accept a reputation delta", errAdminMutationInvalid, in.Kind)
	}
	return nil
}

func adminMutationRequestSHA256(in adminMutationIntent) (string, error) {
	in = in.normalized()
	if err := in.validate(); err != nil {
		return "", err
	}
	raw, err := json.Marshal(in)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

func prepareAdminMutation(actor AdminActor, in adminMutationIntent) (adminMutationIntent, error) {
	if err := validateAdminActorShape(actor); err != nil {
		return in, err
	}
	in = in.normalized()
	if _, err := adminMutationRequestSHA256(in); err != nil {
		return in, err
	}
	return in, nil
}

func insertAdminMutationAction(
	ctx context.Context,
	tx pgx.Tx,
	actor AdminActor,
	intent adminMutationIntent,
	taskID, supplierID, ledgerEntryID *uuid.UUID,
	before, after any,
) error {
	if err := revalidateAdminActor(ctx, tx, actor); err != nil {
		return err
	}
	intent = intent.normalized()
	digest, err := adminMutationRequestSHA256(intent)
	if err != nil {
		return err
	}
	if before == nil || after == nil {
		return fmt.Errorf("%w: before and after audit state are required", errAdminMutationInvalid)
	}
	detail, err := json.Marshal(map[string]any{"before": before, "after": after})
	if err != nil {
		return fmt.Errorf("encode admin mutation audit: %w", err)
	}
	actionID := uuid.New()
	correlationRef := intent.CorrelationRef
	if correlationRef == "" {
		correlationRef = actionID.String()
	}
	label := strings.TrimSpace(actor.Label)
	if len(label) > 200 {
		label = label[:200]
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO admin_actions (
		  id,kind,task_id,supplier_id,ledger_entry_id,reason,detail,
		  actor_mode,actor_principal_id,actor_session_id,actor_label,attribution_scope,
		  intent_version,request_sha256,correlation_ref,target_kind,target_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17)`,
		actionID, intent.Kind, taskID, supplierID, ledgerEntryID, intent.Reason, detail,
		string(actor.Mode), actor.PrincipalID, actor.SessionID, nullIfEmpty(label), string(actor.AttributionScope),
		intent.Version, digest, correlationRef, intent.TargetKind, intent.TargetID)
	return err
}
