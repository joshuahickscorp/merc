package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const (
	controlIntake   = "intake"
	controlDispatch = "dispatch"
	controlPayments = "payments"
	controlWebhooks = "webhooks"
)

var validOperationalControls = map[string]bool{
	controlIntake: true, controlDispatch: true, controlPayments: true, controlWebhooks: true,
}

var operationalControlNamespace = uuid.MustParse("292f9c8c-35b6-4d20-a8a2-60d99724db78")

type OperationalControl struct {
	Name      string     `json:"name"`
	Paused    bool       `json:"paused"`
	Reason    string     `json:"reason"`
	UpdatedAt time.Time  `json:"updated_at"`
	UpdatedBy *uuid.UUID `json:"updated_by,omitempty"`
	Version   int64      `json:"version"`
}

func operationalControlID(name string) uuid.UUID {
	return uuid.NewSHA1(operationalControlNamespace, []byte(name))
}

func normalizeOperationalControl(name string) (string, error) {
	name = strings.ToLower(strings.TrimSpace(name))
	if !validOperationalControls[name] {
		return "", fmt.Errorf("unknown operational control %q", name)
	}
	return name, nil
}

func (s *Store) OperationalControlPaused(ctx context.Context, name string) (bool, error) {
	name, err := normalizeOperationalControl(name)
	if err != nil {
		return true, err
	}
	var paused bool
	err = s.pool.QueryRow(ctx, `SELECT paused FROM operational_controls WHERE name=$1`, name).Scan(&paused)
	if errors.Is(err, pgx.ErrNoRows) {
		return true, fmt.Errorf("operational control %s is missing", name)
	}
	return paused, err
}

func (s *Store) ListOperationalControls(ctx context.Context) ([]OperationalControl, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT name,paused,reason,updated_at,updated_by,version
		FROM operational_controls ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OperationalControl
	for rows.Next() {
		var item OperationalControl
		if err := rows.Scan(&item.Name, &item.Paused, &item.Reason, &item.UpdatedAt, &item.UpdatedBy, &item.Version); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func (s *Store) AdminSetOperationalControl(
	ctx context.Context,
	actor AdminActor,
	name string,
	paused bool,
	reason string,
	correlationRef string,
) (OperationalControl, error) {
	name, err := normalizeOperationalControl(name)
	if err != nil {
		return OperationalControl{}, fmt.Errorf("%w: %v", errAdminMutationInvalid, err)
	}
	intent, err := prepareAdminMutation(actor, adminMutationIntent{
		Kind: adminActionControlChanged, TargetKind: adminTargetControl,
		TargetID: operationalControlID(name), Reason: reason, CorrelationRef: correlationRef,
	})
	if err != nil {
		return OperationalControl{}, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return OperationalControl{}, err
	}
	defer tx.Rollback(ctx)
	if err := revalidateAdminActor(ctx, tx, actor); err != nil {
		return OperationalControl{}, err
	}

	var before OperationalControl
	if err := tx.QueryRow(ctx, `
		SELECT name,paused,reason,updated_at,updated_by,version
		FROM operational_controls WHERE name=$1 FOR UPDATE`, name).Scan(
		&before.Name, &before.Paused, &before.Reason, &before.UpdatedAt, &before.UpdatedBy, &before.Version,
	); errors.Is(err, pgx.ErrNoRows) {
		return OperationalControl{}, errNotFound
	} else if err != nil {
		return OperationalControl{}, err
	}

	var after OperationalControl
	if err := tx.QueryRow(ctx, `
		UPDATE operational_controls
		SET paused=$2,reason=$3,updated_at=now(),updated_by=$4,version=version+1
		WHERE name=$1
		RETURNING name,paused,reason,updated_at,updated_by,version`,
		name, paused, intent.Reason, actor.PrincipalID,
	).Scan(&after.Name, &after.Paused, &after.Reason, &after.UpdatedAt, &after.UpdatedBy, &after.Version); err != nil {
		return OperationalControl{}, err
	}
	if err := insertAdminMutationAction(ctx, tx, actor, intent, nil, nil, nil,
		map[string]any{"name": before.Name, "paused": before.Paused, "reason": before.Reason, "version": before.Version},
		map[string]any{"name": after.Name, "paused": after.Paused, "reason": after.Reason, "version": after.Version}); err != nil {
		return OperationalControl{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return OperationalControl{}, err
	}
	return after, nil
}

type operationalControlRequest struct {
	Paused    *bool  `json:"paused"`
	Reason    string `json:"reason"`
	RequestID string `json:"request_id,omitempty"`
}

func (s *Server) handleAdminControls(w http.ResponseWriter, r *http.Request) {
	controls, err := s.store.ListOperationalControls(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "loading operational controls: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"controls": controls})
}

func (s *Server) handleAdminSetControl(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, adminActionBodyLimit+1))
	if err != nil || len(raw) > adminActionBodyLimit {
		writeErr(w, http.StatusBadRequest, "invalid operational-control request")
		return
	}
	var body operationalControlRequest
	if err := decodeStrictJSONObject(raw, &body); err != nil || body.Paused == nil {
		writeErr(w, http.StatusBadRequest, "paused boolean and reason are required")
		return
	}
	actor, ok := adminActorFromContext(r.Context())
	if !ok {
		writeErr(w, http.StatusUnauthorized, "authenticated admin identity is required")
		return
	}
	control, err := s.store.AdminSetOperationalControl(
		r.Context(), actor, r.PathValue("name"), *body.Paused, body.Reason, body.RequestID)
	if writeAdminMutationInputOrAuthError(w, err) {
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "changing operational control: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, control)
}

func (s *Server) requireOperationalControlActive(w http.ResponseWriter, r *http.Request, name string) bool {
	paused, err := s.store.OperationalControlPaused(r.Context(), name)
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, name+" control unavailable")
		return false
	}
	if paused {
		writeErr(w, http.StatusServiceUnavailable, name+" processing is paused by the operator")
		return false
	}
	return true
}
