package main

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type AdminAuthMode string

const (
	// AdminAuthOperatorKey is the primary offline-minted admin principal stored
	// in admin_credentials (not buyer api_keys).
	AdminAuthOperatorKey AdminAuthMode = "operator_key"
	// AdminAuthBreakGlassAPIKey is the legacy is_admin API-key path kept as an
	// emergency migration fallback. New operator access should use operator_key.
	AdminAuthBreakGlassAPIKey AdminAuthMode = "break_glass_api_key"
)

type AdminAttributionScope string

const (
	AdminAttributionSharedCredentialOnly AdminAttributionScope = "shared_credential_only"
	AdminAttributionNamedOperatorKey     AdminAttributionScope = "named_operator_key"
)

type AdminActor struct {
	Mode             AdminAuthMode         `json:"authentication_mode"`
	PrincipalID      uuid.UUID             `json:"principal_id"`
	SessionID        *uuid.UUID            `json:"session_id,omitempty"`
	AttributionScope AdminAttributionScope `json:"attribution_scope"`
	Label            string                `json:"label,omitempty"`
}

var (
	errAdminActorUnauthorized = errors.New("admin actor is no longer authorized")
	errAdminSourceForbidden   = errors.New("admin source address is not allowlisted")
)

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
	switch actor.Mode {
	case AdminAuthOperatorKey, AdminAuthBreakGlassAPIKey:
	default:
		return fmt.Errorf("%w: unsupported authentication mode %q", errAdminActorUnauthorized, actor.Mode)
	}
	if actor.SessionID != nil || actor.AttributionScope != AdminAttributionNamedOperatorKey {
		return fmt.Errorf("%w: invalid admin key attribution", errAdminActorUnauthorized)
	}
	label := strings.TrimSpace(actor.Label)
	if label == "" || len(label) > 200 {
		return fmt.Errorf("%w: a named operator key label is required", errAdminActorUnauthorized)
	}
	return nil
}

func revalidateAdminActor(ctx context.Context, tx pgx.Tx, actor AdminActor) error {
	if err := validateAdminActorShape(actor); err != nil {
		return err
	}

	var one int
	var err error
	switch actor.Mode {
	case AdminAuthOperatorKey:
		err = tx.QueryRow(ctx, `
			SELECT 1
			  FROM admin_credentials
			 WHERE id = $1 AND revoked = false
			 FOR SHARE`, actor.PrincipalID).Scan(&one)
	case AdminAuthBreakGlassAPIKey:
		err = tx.QueryRow(ctx, `
			SELECT 1
			  FROM api_keys
			 WHERE id = $1 AND is_admin = true AND revoked = false
			 FOR SHARE`, actor.PrincipalID).Scan(&one)
	default:
		return fmt.Errorf("%w: unsupported authentication mode %q", errAdminActorUnauthorized, actor.Mode)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return errAdminActorUnauthorized
	}
	if err != nil {
		return fmt.Errorf("revalidate admin actor: %w", err)
	}
	return nil
}

// parseAdminCIDRs loads MERC_ADMIN_CIDRS the same way canary allowlists are loaded:
// a comma-separated list of CIDR prefixes. Empty means "unset".
func parseAdminCIDRs(raw string) ([]*net.IPNet, error) {
	var out []*net.IPNet
	for _, part := range splitCanaryCSV(raw) {
		_, network, err := net.ParseCIDR(part)
		if err != nil {
			return nil, fmt.Errorf("MERC_ADMIN_CIDRS contains %q: %w", part, err)
		}
		out = append(out, network)
	}
	return out, nil
}

func loadAdminCIDRsFromEnv() ([]*net.IPNet, error) {
	return parseAdminCIDRs(os.Getenv("MERC_ADMIN_CIDRS"))
}

// adminSourceAllowed enforces the MERC_ADMIN_CIDRS IP allowlist. When the
// allowlist is unset, only loopback is accepted so local proofs keep working
// without opening admin routes to the fleet by default. Production startup
// requires an explicit non-empty allowlist (see validateAdminAccessConfig).
func adminSourceAllowed(clientAddr string) bool {
	ip := net.ParseIP(strings.TrimSpace(clientAddr))
	if ip == nil {
		return false
	}
	networks, err := loadAdminCIDRsFromEnv()
	if err != nil {
		return false
	}
	if len(networks) == 0 {
		return ip.IsLoopback()
	}
	for _, network := range networks {
		if network.Contains(ip) {
			return true
		}
	}
	return false
}

func validateAdminAccessConfig(cxEnv, adminCIDRs string) error {
	if _, err := parseAdminCIDRs(adminCIDRs); err != nil {
		return err
	}
	production := strings.EqualFold(cxEnv, "production") || strings.EqualFold(cxEnv, "prod")
	if production && strings.TrimSpace(adminCIDRs) == "" {
		return fmt.Errorf("MERC_ADMIN_CIDRS is required in production (comma-separated operator management CIDRs)")
	}
	return nil
}

// constantTimeHashEqual compares two hex digests without early exit on the
// first differing byte. Lengths must match for a true result.
func constantTimeHashEqual(presented, stored string) bool {
	if len(presented) != len(stored) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(presented), []byte(stored)) == 1
}

// AuthenticateAdmin resolves a bearer credential to an admin principal.
// Primary path: admin_credentials (offline-minted operator keys).
// Migration path: api_keys.is_admin break-glass keys remain accepted until revoked.
func (s *Store) AuthenticateAdmin(ctx context.Context, rawKey string) (AdminActor, error) {
	presented := hashKey(rawKey)

	var (
		id     uuid.UUID
		stored string
		label  string
	)
	err := s.pool.QueryRow(ctx, `
		SELECT id, key_hash, label
		  FROM admin_credentials
		 WHERE key_hash = $1 AND revoked = false`, presented,
	).Scan(&id, &stored, &label)
	if err == nil && constantTimeHashEqual(presented, stored) {
		return AdminActor{
			Mode:             AdminAuthOperatorKey,
			PrincipalID:      id,
			AttributionScope: AdminAttributionNamedOperatorKey,
			Label:            label,
		}, nil
	}
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return AdminActor{}, err
	}

	// Break-glass migration: legacy is_admin API keys still authorize until
	// operators mint admin_credentials and revoke the old rows.
	err = s.pool.QueryRow(ctx, `
		SELECT id, key_hash,
		       COALESCE(NULLIF(name,''), 'break-glass API key')
		  FROM api_keys
		 WHERE key_hash = $1 AND is_admin = true AND revoked = false`, presented,
	).Scan(&id, &stored, &label)
	if errors.Is(err, pgx.ErrNoRows) {
		return AdminActor{}, errNotFound
	}
	if err != nil {
		return AdminActor{}, err
	}
	if !constantTimeHashEqual(presented, stored) {
		return AdminActor{}, errNotFound
	}
	return AdminActor{
		Mode:             AdminAuthBreakGlassAPIKey,
		PrincipalID:      id,
		AttributionScope: AdminAttributionNamedOperatorKey,
		Label:            label,
	}, nil
}
