package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const moneyAuthorityIntentVersion = 1

var errMoneyAuthorityAuditInvariant = errors.New("money authority audit invariant violated")

type moneyAuthorityIntent struct {
	Version             int       `json:"version"`
	Kind                string    `json:"kind"`
	TargetKind          string    `json:"target_kind"`
	TargetID            uuid.UUID `json:"target_id"`
	FundID              uuid.UUID `json:"fund_id"`
	FundRef             string    `json:"fund_ref"`
	ExternalTreasuryRef string    `json:"external_treasury_ref,omitempty"`
	AuthorizationRef    string    `json:"authorization_ref,omitempty"`
	AmountCents         int64     `json:"amount_cents"`
	Currency            string    `json:"currency"`
	Reason              string    `json:"reason"`
	CorrelationRef      string    `json:"correlation_ref"`
}

func (in moneyAuthorityIntent) normalized() moneyAuthorityIntent {
	in.Version = moneyAuthorityIntentVersion
	in.Kind = strings.TrimSpace(in.Kind)
	in.TargetKind = strings.TrimSpace(in.TargetKind)
	in.FundRef = strings.TrimSpace(in.FundRef)
	in.ExternalTreasuryRef = strings.TrimSpace(in.ExternalTreasuryRef)
	in.AuthorizationRef = strings.TrimSpace(in.AuthorizationRef)
	in.Currency = strings.ToLower(strings.TrimSpace(in.Currency))
	in.Reason = strings.TrimSpace(in.Reason)
	in.CorrelationRef = strings.TrimSpace(in.CorrelationRef)
	return in
}

func (in moneyAuthorityIntent) validate() error {
	if in.Version != moneyAuthorityIntentVersion {
		return fmt.Errorf("unsupported money authority intent version %d", in.Version)
	}
	if in.Kind != "subsidy_fund_authorized" && in.Kind != "payout_subsidy_authorized" {
		return fmt.Errorf("unsupported money authority action %q", in.Kind)
	}
	if in.TargetKind == "" || in.TargetID == uuid.Nil || in.FundID == uuid.Nil {
		return errors.New("money authority target and fund ids are required")
	}
	if in.FundRef == "" || in.CorrelationRef == "" || in.Reason == "" {
		return errors.New("money authority fund, correlation, and reason are required")
	}
	if in.AmountCents <= 0 || in.Currency != "usd" {
		return errors.New("money authority requires positive integer USD cents")
	}
	switch in.Kind {
	case "subsidy_fund_authorized":
		if in.TargetKind != "subsidy_fund" || in.TargetID != in.FundID ||
			in.CorrelationRef != in.FundRef || in.ExternalTreasuryRef == "" || in.AuthorizationRef != "" {
			return errors.New("invalid subsidy-fund authorization binding")
		}
	case "payout_subsidy_authorized":
		if in.TargetKind != "supplier_liability" || in.AuthorizationRef == "" ||
			in.CorrelationRef != in.AuthorizationRef || in.ExternalTreasuryRef != "" {
			return errors.New("invalid payout-subsidy authorization binding")
		}
	}
	return nil
}

func moneyAuthorityRequestSHA256(in moneyAuthorityIntent) (string, error) {
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

func insertMoneyAuthorityAction(
	ctx context.Context,
	tx pgx.Tx,
	actor AdminActor,
	actionID uuid.UUID,
	intent moneyAuthorityIntent,
	supplierID *uuid.UUID,
) (string, error) {
	if actionID == uuid.Nil {
		return "", errors.New("money authority action id is required")
	}
	if err := revalidateAdminActor(ctx, tx, actor); err != nil {
		return "", err
	}
	intent = intent.normalized()
	digest, err := moneyAuthorityRequestSHA256(intent)
	if err != nil {
		return "", err
	}
	label := strings.TrimSpace(actor.Label)
	if len(label) > 200 {
		label = label[:200]
	}
	var ledgerEntryID *uuid.UUID
	if intent.TargetKind == "supplier_liability" {
		id := intent.TargetID
		ledgerEntryID = &id
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO admin_actions (
		  id,kind,supplier_id,ledger_entry_id,reason,
		  actor_mode,actor_principal_id,actor_session_id,actor_label,attribution_scope,
		  intent_version,request_sha256,correlation_ref,target_kind,target_id,
		  fund_id,fund_ref,authorization_ref,amount_cents,currency,detail)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,NULL)`,
		actionID, intent.Kind, supplierID, ledgerEntryID, intent.Reason,
		string(actor.Mode), actor.PrincipalID, actor.SessionID, nullIfEmpty(label), string(actor.AttributionScope),
		intent.Version, digest, intent.CorrelationRef, intent.TargetKind, intent.TargetID,
		intent.FundID, intent.FundRef, nullIfEmpty(intent.AuthorizationRef), intent.AmountCents, intent.Currency)
	if err != nil {
		return "", err
	}
	return digest, nil
}

func assertMoneyAuthorityAction(
	ctx context.Context,
	tx pgx.Tx,
	actor AdminActor,
	actionID uuid.UUID,
	intent moneyAuthorityIntent,
) error {
	if actionID == uuid.Nil {
		return fmt.Errorf("%w: resource has no authorization action", errMoneyAuthorityAuditInvariant)
	}
	if err := revalidateAdminActor(ctx, tx, actor); err != nil {
		return err
	}
	intent = intent.normalized()
	digest, err := moneyAuthorityRequestSHA256(intent)
	if err != nil {
		return err
	}
	var (
		kind, requestSHA, correlationRef, targetKind, fundRef string
		authorizationRef, currency, reason                    string
		targetID, fundID                                      uuid.UUID
		amountCents                                           int64
	)
	err = tx.QueryRow(ctx, `
		SELECT kind,request_sha256,correlation_ref,target_kind,target_id,
		       fund_id,fund_ref,COALESCE(authorization_ref,''),amount_cents,currency,COALESCE(reason,'')
		  FROM admin_actions WHERE id=$1 FOR SHARE`, actionID).Scan(
		&kind, &requestSHA, &correlationRef, &targetKind, &targetID,
		&fundID, &fundRef, &authorizationRef, &amountCents, &currency, &reason)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: linked action %s is missing", errMoneyAuthorityAuditInvariant, actionID)
	}
	if err != nil {
		return err
	}
	if kind != intent.Kind || requestSHA != digest || correlationRef != intent.CorrelationRef ||
		targetKind != intent.TargetKind || targetID != intent.TargetID || fundID != intent.FundID ||
		fundRef != intent.FundRef || authorizationRef != intent.AuthorizationRef ||
		amountCents != intent.AmountCents || currency != intent.Currency || reason != intent.Reason {
		return fmt.Errorf("%w: linked action %s does not match the normalized request", errMoneyAuthorityAuditInvariant, actionID)
	}
	return nil
}
