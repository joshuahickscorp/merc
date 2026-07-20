package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type AdminAuthMode string

const AdminAuthBreakGlassAPIKey AdminAuthMode = "break_glass_api_key"

type AdminAttributionScope string

const AdminAttributionSharedCredentialOnly AdminAttributionScope = "shared_credential_only"

type AdminActor struct {
	Mode             AdminAuthMode         `json:"authentication_mode"`
	PrincipalID      uuid.UUID             `json:"principal_id"`
	SessionID        *uuid.UUID            `json:"session_id,omitempty"`
	AttributionScope AdminAttributionScope `json:"attribution_scope"`
	Label            string                `json:"label,omitempty"`
}

var errAdminActorUnauthorized = errors.New("admin actor is no longer authorized")

func nullIfEmpty(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func adminActorFromContext(ctx context.Context) (AdminActor, bool) {
	actor, ok := ctx.Value(ctxAdmin).(AdminActor)
	if !ok || validateAdminActorShape(actor) != nil {
		return AdminActor{}, false
	}
	return actor, true
}

func validateAdminActorShape(actor AdminActor) error {
	if actor.PrincipalID == uuid.Nil {
		return fmt.Errorf("%w: missing principal id", errAdminActorUnauthorized)
	}
	if actor.Mode != AdminAuthBreakGlassAPIKey {
		return fmt.Errorf("%w: unsupported authentication mode %q", errAdminActorUnauthorized, actor.Mode)
	}
	if actor.SessionID != nil || actor.AttributionScope != AdminAttributionSharedCredentialOnly {
		return fmt.Errorf("%w: invalid admin key attribution", errAdminActorUnauthorized)
	}
	return nil
}

func revalidateAdminActor(ctx context.Context, tx pgx.Tx, actor AdminActor) error {
	if err := validateAdminActorShape(actor); err != nil {
		return err
	}

	var one int
	err := tx.QueryRow(ctx, `
			SELECT 1
			  FROM api_keys
			 WHERE id = $1 AND is_admin = true AND revoked = false
			 FOR SHARE`, actor.PrincipalID).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return errAdminActorUnauthorized
	}
	if err != nil {
		return fmt.Errorf("revalidate admin actor: %w", err)
	}
	return nil
}
