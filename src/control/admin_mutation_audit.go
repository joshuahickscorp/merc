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
	adminActionBuyerTombstoned   = "buyer_tombstoned"
	adminActionRealtimeRefunded  = "realtime_refunded"
	adminActionPrepaidRefunded   = "prepaid_refunded"
	adminActionDisputeResolved   = "dispute_resolved"

	adminTargetWorker      = "worker"
	adminTargetTask        = "task"
	adminTargetSupplier    = "supplier"
	adminTargetLedgerEntry = "ledger_entry"
	adminTargetControl     = "operational_control"
	adminTargetBuyer       = "buyer"
	adminTargetContract    = "execution_contract"
	adminTargetDispute     = "dispute"
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
	// Resolution is required for dispute_resolved (upheld|rejected) so a retry
	// with the same correlation_ref but a different verdict cannot no-op as a
	// same-request replay.
	Resolution string `json:"resolution,omitempty"`
}

type adminMutationReplay struct {
	Found  bool
	Detail json.RawMessage
}

func (in adminMutationIntent) normalized() adminMutationIntent {
	in.Version = adminMutationIntentVersion
	in.Kind = strings.TrimSpace(in.Kind)
	in.TargetKind = strings.TrimSpace(in.TargetKind)
	in.Reason = strings.TrimSpace(in.Reason)
	in.CorrelationRef = strings.TrimSpace(in.CorrelationRef)
	in.Resolution = strings.TrimSpace(in.Resolution)
	return in
}

func (in adminMutationIntent) validate() error {
	if in.Version != adminMutationIntentVersion || in.TargetID == uuid.Nil {
		return fmt.Errorf("%w: version and target are required", errAdminMutationInvalid)
	}
	if in.Reason == "" || in.CorrelationRef == "" {
		return fmt.Errorf("%w: reason and incident or ticket reference are required", errAdminMutationInvalid)
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
	case adminActionBuyerTombstoned, adminActionPrepaidRefunded:
		wantTarget = adminTargetBuyer
	case adminActionRealtimeRefunded:
		wantTarget = adminTargetContract
	case adminActionDisputeResolved:
		wantTarget = adminTargetDispute
		res := strings.TrimSpace(in.Resolution)
		if res != "upheld" && res != "rejected" {
			return fmt.Errorf("%w: dispute resolution must be upheld or rejected", errAdminMutationInvalid)
		}
	default:
		return fmt.Errorf("%w: unsupported action %q", errAdminMutationInvalid, in.Kind)
	}
	if in.TargetKind != wantTarget {
		return fmt.Errorf("%w: action %q requires target kind %q", errAdminMutationInvalid, in.Kind, wantTarget)
	}
	if in.Kind != adminActionReputationChanged && in.Delta != nil {
		return fmt.Errorf("%w: action %q does not accept a reputation delta", errAdminMutationInvalid, in.Kind)
	}
	if in.Kind != adminActionDisputeResolved && strings.TrimSpace(in.Resolution) != "" {
		return fmt.Errorf("%w: action %q does not accept a dispute resolution", errAdminMutationInvalid, in.Kind)
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

// acquireAdminMutationReplay serializes use of a privileged-action correlation
// reference. An exact retry by the same named operator is a no-op; any reuse for
// a different request or operator fails closed.
func acquireAdminMutationReplay(
	ctx context.Context,
	tx pgx.Tx,
	actor AdminActor,
	intent adminMutationIntent,
) (adminMutationReplay, error) {
	intent = intent.normalized()
	digest, err := adminMutationRequestSHA256(intent)
	if err != nil {
		return adminMutationReplay{}, err
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`,
		intent.Kind+"|"+intent.CorrelationRef); err != nil {
		return adminMutationReplay{}, err
	}
	var storedDigest, storedLabel, storedScope string
	var storedPrincipal uuid.UUID
	var detail json.RawMessage
	err = tx.QueryRow(ctx, `
		SELECT request_sha256,actor_principal_id,COALESCE(actor_label,''),
		       COALESCE(attribution_scope,''),detail
		  FROM admin_actions
		 WHERE kind=$1 AND correlation_ref=$2`,
		intent.Kind, intent.CorrelationRef).Scan(
		&storedDigest, &storedPrincipal, &storedLabel, &storedScope, &detail)
	if errors.Is(err, pgx.ErrNoRows) {
		return adminMutationReplay{}, nil
	}
	if err != nil {
		return adminMutationReplay{}, err
	}
	if storedDigest != digest || storedPrincipal != actor.PrincipalID ||
		storedLabel != strings.TrimSpace(actor.Label) || storedScope != string(actor.AttributionScope) {
		return adminMutationReplay{}, fmt.Errorf(
			"%w: incident or ticket reference already belongs to another privileged request",
			errAdminMutationInvalid)
	}
	return adminMutationReplay{Found: true, Detail: detail}, nil
}

func replayedReputation(detail json.RawMessage) (before, after float32, err error) {
	var state struct {
		Before struct {
			Reputation float32 `json:"reputation"`
		} `json:"before"`
		After struct {
			Reputation float32 `json:"reputation"`
		} `json:"after"`
	}
	if err := json.Unmarshal(detail, &state); err != nil {
		return 0, 0, fmt.Errorf("decode privileged-action replay: %w", err)
	}
	return state.Before.Reputation, state.After.Reputation, nil
}

func insertAdminMutationAction(
	ctx context.Context,
	tx pgx.Tx,
	actor AdminActor,
	intent adminMutationIntent,
	taskID, supplierID, ledgerEntryID *uuid.UUID,
	before, after any,
) error {
	_, err := insertAdminMutationActionWithID(ctx, tx, actor, intent, taskID, supplierID,
		ledgerEntryID, before, after)
	return err
}

func insertAdminMutationActionWithID(
	ctx context.Context,
	tx pgx.Tx,
	actor AdminActor,
	intent adminMutationIntent,
	taskID, supplierID, ledgerEntryID *uuid.UUID,
	before, after any,
) (uuid.UUID, error) {
	if err := revalidateAdminActor(ctx, tx, actor); err != nil {
		return uuid.Nil, err
	}
	intent = intent.normalized()
	digest, err := adminMutationRequestSHA256(intent)
	if err != nil {
		return uuid.Nil, err
	}
	if before == nil || after == nil {
		return uuid.Nil, fmt.Errorf("%w: before and after audit state are required", errAdminMutationInvalid)
	}
	detail, err := json.Marshal(map[string]any{"before": before, "after": after})
	if err != nil {
		return uuid.Nil, fmt.Errorf("encode admin mutation audit: %w", err)
	}
	actionID := uuid.New()
	correlationRef := intent.CorrelationRef
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
	if err != nil {
		return uuid.Nil, err
	}
	return actionID, nil
}
