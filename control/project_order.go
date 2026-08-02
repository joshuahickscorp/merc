package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ProjectOrder is the durable monetary authority for a buyer-approved project.
// It deliberately stores no source tree, prompt, or materialized output: those
// remain buyer-controlled artifacts and are bound at each firm-job boundary.
type ProjectOrder struct {
	ID                string             `json:"id"`
	IRSHA256          string             `json:"ir_sha256"`
	Currency          string             `json:"currency"`
	BuyerCeilingNanos int64              `json:"buyer_ceiling_nanos"`
	ReservedNanos     int64              `json:"reserved_nanos"`
	RemainingNanos    int64              `json:"remaining_nanos"`
	Status            string             `json:"status"`
	CreatedAt         time.Time          `json:"created_at"`
	Steps             []ProjectOrderStep `json:"steps"`
}

// ProjectOrderStep is server-side evidence that a specific firm job consumed a
// named project slot. It is deliberately returned on the buyer-scoped order
// read so a later dependent quote can reject a fabricated local hand-off
// receipt before it sends its own input to /v1/quote.
type ProjectOrderStep struct {
	StepID                string `json:"step_id"`
	JobID                 string `json:"job_id"`
	QuoteID               string `json:"quote_id"`
	PricingDecisionSHA256 string `json:"pricing_decision_sha256"`
	AcceptedCeilingNanos  int64  `json:"accepted_ceiling_nanos"`
}

type projectOrderCreateRequest struct {
	IRSHA256          string `json:"ir_sha256"`
	Currency          string `json:"currency"`
	BuyerCeilingNanos int64  `json:"buyer_ceiling_nanos"`
}

var (
	errProjectOrderNotFound = errors.New("project order not found")
	errProjectBudget        = errors.New("project accepted ceilings exceed buyer ceiling")
	errProjectStepReserved  = errors.New("project step already has an accepted job")
)

func validateProjectOrderRequest(in projectOrderCreateRequest) (Currency, error) {
	if !validSHA256(in.IRSHA256) {
		return Currency{}, errors.New("project order requires an exact Project IR sha256")
	}
	if in.BuyerCeilingNanos <= 0 {
		return Currency{}, errors.New("project buyer ceiling must be positive nanos")
	}
	currency, err := ParseCurrency(in.Currency)
	if err != nil {
		return Currency{}, fmt.Errorf("project currency: %w", err)
	}
	if err := RequireSettlementCurrency(currency.Code()); err != nil {
		return Currency{}, fmt.Errorf("project settlement currency: %w", err)
	}
	return currency, nil
}

// CreateProjectOrder is idempotent for the buyer-provided request key. A replay
// with altered IR or ceiling is refused rather than returning the old order.
func (s *Store) CreateProjectOrder(
	ctx context.Context, buyerID uuid.UUID, in projectOrderCreateRequest,
	idempotencyKey, requestSHA string,
) (ProjectOrder, bool, error) {
	if _, err := validateProjectOrderRequest(in); err != nil {
		return ProjectOrder{}, false, err
	}
	if !idempotencyKeyPattern.MatchString(idempotencyKey) || !validSHA256(requestSHA) {
		return ProjectOrder{}, false, errors.New("project order requires valid idempotency authority")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return ProjectOrder{}, false, err
	}
	defer tx.Rollback(ctx)
	var existing ProjectOrder
	err = tx.QueryRow(ctx, `
		SELECT id,ir_sha256,currency,buyer_ceiling_nanos,status,created_at
		  FROM project_orders
		 WHERE buyer_id=$1 AND create_idempotency_key=$2
		 FOR UPDATE`, buyerID, idempotencyKey,
	).Scan(&existing.ID, &existing.IRSHA256, &existing.Currency, &existing.BuyerCeilingNanos,
		&existing.Status, &existing.CreatedAt)
	if err == nil {
		var storedSHA string
		if err := tx.QueryRow(ctx, `SELECT create_request_sha256 FROM project_orders WHERE id=$1`, existing.ID).Scan(&storedSHA); err != nil {
			return ProjectOrder{}, false, err
		}
		if storedSHA != requestSHA {
			return ProjectOrder{}, false, errors.New("project order idempotency key was reused with a different request")
		}
		if err := fillProjectOrderReservation(ctx, tx, &existing); err != nil {
			return ProjectOrder{}, false, err
		}
		if err := tx.Commit(ctx); err != nil {
			return ProjectOrder{}, false, err
		}
		return existing, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return ProjectOrder{}, false, err
	}
	out := ProjectOrder{ID: uuid.NewString(), IRSHA256: in.IRSHA256, Currency: strings.ToLower(in.Currency),
		BuyerCeilingNanos: in.BuyerCeilingNanos, RemainingNanos: in.BuyerCeilingNanos, Status: "OPEN"}
	if err := tx.QueryRow(ctx, `
		INSERT INTO project_orders
		  (id,buyer_id,ir_sha256,currency,buyer_ceiling_nanos,create_idempotency_key,create_request_sha256)
		VALUES ($1,$2,$3,$4,$5,$6,$7)
		RETURNING created_at`, out.ID, buyerID, out.IRSHA256, out.Currency, out.BuyerCeilingNanos,
		idempotencyKey, requestSHA).Scan(&out.CreatedAt); err != nil {
		return ProjectOrder{}, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return ProjectOrder{}, false, err
	}
	return out, false, nil
}

func fillProjectOrderReservation(ctx context.Context, q pgx.Tx, out *ProjectOrder) error {
	if err := q.QueryRow(ctx, `
		SELECT COALESCE(SUM(accepted_ceiling_nanos),0)
		  FROM project_order_steps WHERE project_order_id=$1`, out.ID).Scan(&out.ReservedNanos); err != nil {
		return err
	}
	if out.ReservedNanos < 0 || out.ReservedNanos > out.BuyerCeilingNanos {
		return errors.New("project order reservation ledger is invalid")
	}
	out.RemainingNanos = out.BuyerCeilingNanos - out.ReservedNanos
	rows, err := q.Query(ctx, `
		SELECT step_id,job_id,quote_id,pricing_decision_sha256,accepted_ceiling_nanos
		  FROM project_order_steps WHERE project_order_id=$1 ORDER BY step_id`, out.ID)
	if err != nil {
		return err
	}
	defer rows.Close()
	out.Steps = out.Steps[:0]
	for rows.Next() {
		var step ProjectOrderStep
		var quoteID uuid.UUID
		if err := rows.Scan(&step.StepID, &step.JobID, &quoteID,
			&step.PricingDecisionSHA256, &step.AcceptedCeilingNanos); err != nil {
			return err
		}
		step.QuoteID = "q_" + quoteID.String()
		out.Steps = append(out.Steps, step)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return nil
}

func (s *Store) GetProjectOrder(ctx context.Context, buyerID, id uuid.UUID) (ProjectOrder, error) {
	var out ProjectOrder
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return ProjectOrder{}, err
	}
	defer tx.Rollback(ctx)
	err = tx.QueryRow(ctx, `
		SELECT id,ir_sha256,currency,buyer_ceiling_nanos,status,created_at
		  FROM project_orders WHERE id=$1 AND buyer_id=$2 FOR UPDATE`, id, buyerID,
	).Scan(&out.ID, &out.IRSHA256, &out.Currency, &out.BuyerCeilingNanos, &out.Status, &out.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return ProjectOrder{}, errProjectOrderNotFound
	}
	if err != nil {
		return ProjectOrder{}, err
	}
	if err := fillProjectOrderReservation(ctx, tx, &out); err != nil {
		return ProjectOrder{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return ProjectOrder{}, err
	}
	return out, nil
}

// reserveProjectStepTx is called inside the job's admission transaction, after
// the exact PricingDecision is validated and before the job becomes visible.
// Locking the parent order serializes all step reservations for that project.
func reserveProjectStepTx(ctx context.Context, tx pgx.Tx, j *jobRow, currency string, acceptedCeiling int64, pricingSHA string) error {
	if j.ProjectOrderID == uuid.Nil && j.ProjectStepID == "" {
		return nil
	}
	if j.ProjectOrderID == uuid.Nil || !projectStepIDPattern.MatchString(j.ProjectStepID) {
		return errors.New("project jobs require both project_id and project_step_id")
	}
	if !j.FirmQuote || j.QuoteID == uuid.Nil || acceptedCeiling <= 0 || !validSHA256(pricingSHA) {
		return errors.New("project step requires a firm quote with a fixed-point PricingDecision ceiling")
	}
	var owner uuid.UUID
	var orderCurrency, status string
	var ceiling int64
	if err := tx.QueryRow(ctx, `
		SELECT buyer_id,currency,buyer_ceiling_nanos,status
		  FROM project_orders WHERE id=$1 FOR UPDATE`, j.ProjectOrderID,
	).Scan(&owner, &orderCurrency, &ceiling, &status); errors.Is(err, pgx.ErrNoRows) {
		return errProjectOrderNotFound
	} else if err != nil {
		return err
	}
	if owner != j.BuyerID {
		return errors.New("project order does not belong to this buyer")
	}
	if orderCurrency != currency {
		return fmt.Errorf("project order currency %s does not match job currency %s", orderCurrency, currency)
	}
	if status != "OPEN" {
		return fmt.Errorf("project order is not open: %s", status)
	}
	var exists bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS(SELECT 1 FROM project_order_steps WHERE project_order_id=$1 AND step_id=$2)`,
		j.ProjectOrderID, j.ProjectStepID).Scan(&exists); err != nil {
		return err
	}
	if exists {
		return fmt.Errorf("%w: %s", errProjectStepReserved, j.ProjectStepID)
	}
	var reserved int64
	if err := tx.QueryRow(ctx, `
		SELECT COALESCE(SUM(accepted_ceiling_nanos),0)
		  FROM project_order_steps WHERE project_order_id=$1`, j.ProjectOrderID).Scan(&reserved); err != nil {
		return err
	}
	if reserved < 0 || reserved > ceiling || acceptedCeiling > ceiling-reserved {
		return fmt.Errorf("%w: reserved=%d new=%d ceiling=%d", errProjectBudget, reserved, acceptedCeiling, ceiling)
	}
	return nil
}

func (s *Server) handleCreateProjectOrder(w http.ResponseWriter, r *http.Request) {
	auth := r.Context().Value(ctxBuyer).(*AuthResult)
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if !idempotencyKeyPattern.MatchString(key) {
		writeErr(w, http.StatusBadRequest, "Idempotency-Key is required and must be 8-128 characters using letters, digits, '.', '_', ':', or '-'")
		return
	}
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "reading project order: "+err.Error())
		return
	}
	var in projectOrderCreateRequest
	if err := decodeStrictJSONObject(raw, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid project order json: "+err.Error())
		return
	}
	digest := sha256.Sum256(raw)
	out, replay, err := s.store.CreateProjectOrder(r.Context(), auth.BuyerID, in, key, hex.EncodeToString(digest[:]))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if replay {
		w.Header().Set("Idempotent-Replayed", "true")
	}
	writeJSON(w, http.StatusCreated, out)
}

func (s *Server) handleGetProjectOrder(w http.ResponseWriter, r *http.Request) {
	auth := r.Context().Value(ctxBuyer).(*AuthResult)
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid project id")
		return
	}
	out, err := s.store.GetProjectOrder(r.Context(), auth.BuyerID, id)
	if errors.Is(err, errProjectOrderNotFound) {
		writeErr(w, http.StatusNotFound, "project order not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "reading project order: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, out)
}
