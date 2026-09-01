package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

var (
	errSupplierAccountRequired   = errors.New("supplier routes require a self-serve buyer account")
	errSupplierOwnershipConflict = errors.New("supplier ownership conflict")
	errSupplierBodyMustBeEmpty   = errors.New("supplier request must not contain identity or KYC fields")
)

func (s *Store) EnsureSupplierForBuyer(ctx context.Context, buyerID uuid.UUID) (uuid.UUID, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return uuid.Nil, err
	}
	defer tx.Rollback(ctx)

	var email string
	err = tx.QueryRow(ctx,
		`SELECT lower(email) FROM buyers WHERE id = $1 FOR SHARE`, buyerID,
	).Scan(&email)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, errSupplierAccountRequired
	}
	if err != nil {
		return uuid.Nil, err
	}
	if !looksLikeEmail(email) {
		return uuid.Nil, errSupplierAccountRequired
	}

	var supplierID uuid.UUID
	err = tx.QueryRow(ctx,
		`SELECT id FROM suppliers WHERE owner_buyer_id = $1 FOR UPDATE`, buyerID,
	).Scan(&supplierID)
	if err == nil {
		if err := tx.Commit(ctx); err != nil {
			return uuid.Nil, err
		}
		return supplierID, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, err
	}

	var existingOwner *uuid.UUID
	err = tx.QueryRow(ctx,
		`SELECT id, owner_buyer_id
		   FROM suppliers
		  WHERE lower(email) = lower($1)
		  ORDER BY created_at, id
		  LIMIT 1
		  FOR UPDATE`, email,
	).Scan(&supplierID, &existingOwner)
	if err == nil {
		if existingOwner != nil && *existingOwner == buyerID {
			if err := tx.Commit(ctx); err != nil {
				return uuid.Nil, err
			}
			return supplierID, nil
		}
		return uuid.Nil, errSupplierOwnershipConflict
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, err
	}

	err = tx.QueryRow(ctx,
		`INSERT INTO suppliers (email, owner_buyer_id, status)
		 VALUES ($1, $2, 'pending')
		 ON CONFLICT DO NOTHING
		 RETURNING id`, email, buyerID,
	).Scan(&supplierID)
	if err == nil {
		if err := tx.Commit(ctx); err != nil {
			return uuid.Nil, err
		}
		return supplierID, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, err
	}

	err = tx.QueryRow(ctx,
		`SELECT id FROM suppliers WHERE owner_buyer_id = $1`, buyerID,
	).Scan(&supplierID)
	if err == nil {
		if err := tx.Commit(ctx); err != nil {
			return uuid.Nil, err
		}
		return supplierID, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, err
	}
	return uuid.Nil, errSupplierOwnershipConflict
}

func (s *Store) SupplierStatusForBuyer(ctx context.Context, buyerID uuid.UUID) (supplierID uuid.UUID, acct string, payoutsEnabled bool, err error) {
	var acctP *string
	err = s.pool.QueryRow(ctx,
		`SELECT COALESCE(s.id, '00000000-0000-0000-0000-000000000000'::uuid),
		        s.stripe_acct,
		        COALESCE(s.payouts_enabled, false)
		   FROM buyers b
		   LEFT JOIN suppliers s ON s.owner_buyer_id = b.id
		  WHERE b.id = $1`, buyerID,
	).Scan(&supplierID, &acctP, &payoutsEnabled)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, "", false, errSupplierAccountRequired
	}
	if err != nil {
		return uuid.Nil, "", false, err
	}
	if supplierID == uuid.Nil {
		return uuid.Nil, "", false, errNotFound
	}
	if acctP != nil {
		acct = strings.TrimSpace(*acctP)
		if acct != "" && !validStripeObjectID(acct, "acct_") {
			return uuid.Nil, "", false, errors.New("stored Stripe connected account id is not an acct_* identifier")
		}
	}
	return supplierID, acct, payoutsEnabled, nil
}

func (s *Store) CreateWorkerTokenForBuyer(ctx context.Context, buyerID, workerID, supplierID uuid.UUID) (string, error) {
	var owned bool
	if err := s.pool.QueryRow(ctx,
		`SELECT EXISTS (
		   SELECT 1 FROM suppliers WHERE id = $1 AND owner_buyer_id = $2
		 )`, supplierID, buyerID,
	).Scan(&owned); err != nil {
		return "", err
	}
	if !owned {
		return "", errSupplierOwnershipConflict
	}
	return s.CreateWorkerToken(ctx, workerID, supplierID)
}

func decodeEmptySupplierBody(r *http.Request) error {
	if r.Body == nil {
		return nil
	}
	dec := json.NewDecoder(io.LimitReader(r.Body, 4097))
	var body map[string]json.RawMessage
	if err := dec.Decode(&body); errors.Is(err, io.EOF) {
		return nil
	} else if err != nil {
		return err
	}
	if body == nil || len(body) != 0 {
		return errSupplierBodyMustBeEmpty
	}
	var extra json.RawMessage
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("supplier request must contain one JSON object")
		}
		return err
	}
	return nil
}

func writeSupplierStoreError(w http.ResponseWriter, action string, err error) {
	switch {
	case errors.Is(err, errSupplierAccountRequired):
		writeErr(w, http.StatusForbidden, errSupplierAccountRequired.Error())
	case errors.Is(err, errSupplierOwnershipConflict):
		writeErr(w, http.StatusConflict, "supplier ownership requires operator review")
	case errors.Is(err, errStripeAcctTaken):
		writeErr(w, http.StatusConflict, errStripeAcctTaken.Error())
	case errors.Is(err, errWorkerTokenUnboundForbidden):
		writeErr(w, http.StatusForbidden, errWorkerTokenUnboundForbidden.Error())
	default:
		writeErr(w, http.StatusInternalServerError, action+": "+err.Error())
	}
}

func (s *Server) handleSupplierOnboard(w http.ResponseWriter, r *http.Request) {
	if !s.requireOperationalControlActive(w, r, controlPayments) {
		return
	}
	if err := decodeEmptySupplierBody(r); err != nil {
		writeErr(w, http.StatusBadRequest, "supplier identity comes from the authenticated account and KYC is collected by Stripe; send an empty JSON object")
		return
	}
	auth := r.Context().Value(ctxBuyer).(*AuthResult)
	supplierID, err := s.store.EnsureSupplierForBuyer(r.Context(), auth.BuyerID)
	if err != nil {
		writeSupplierStoreError(w, "recording supplier", err)
		return
	}

	acct, err := ensureConnectAccount(r.Context(), s.store, supplierID)
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	link, err := onboardingLink(r.Context(), acct)
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"supplier_id":    supplierID,
		"account":        acct,
		"onboarding_url": link,
	})
}

func (s *Server) handleCreateWorkerToken(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	workerID := uuid.New()
	if s.canary.Enabled {
		var body struct {
			WorkerID uuid.UUID `json:"worker_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.WorkerID == uuid.Nil {
			writeErr(w, http.StatusBadRequest, "private-canary worker token requires an approved worker_id")
			return
		}
		if !s.canary.allowsWorker(body.WorkerID) {
			writeErr(w, http.StatusForbidden, "worker is not approved for this private canary")
			return
		}
		workerID = body.WorkerID
	} else if err := decodeEmptySupplierBody(r); err != nil {
		writeErr(w, http.StatusBadRequest, "supplier identity comes from the authenticated account; send an empty JSON object")
		return
	}
	auth := r.Context().Value(ctxBuyer).(*AuthResult)
	supplierID, err := s.store.EnsureSupplierForBuyer(r.Context(), auth.BuyerID)
	if err != nil {
		writeSupplierStoreError(w, "recording supplier", err)
		return
	}
	token, err := s.store.CreateWorkerTokenForBuyer(r.Context(), auth.BuyerID, workerID, supplierID)
	if err != nil {
		writeSupplierStoreError(w, "minting worker token", err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"supplier_id":  supplierID,
		"worker_id":    workerID,
		"worker_token": token,
	})
}

func (s *Server) handleSupplierStatus(w http.ResponseWriter, r *http.Request) {
	if _, supplied := r.URL.Query()["email"]; supplied {
		writeErr(w, http.StatusBadRequest, "email is not accepted; supplier status is scoped to the authenticated account")
		return
	}
	auth := r.Context().Value(ctxBuyer).(*AuthResult)
	supplierID, acct, payoutsEnabled, err := s.store.SupplierStatusForBuyer(r.Context(), auth.BuyerID)
	if errors.Is(err, errNotFound) {
		writeErr(w, http.StatusNotFound, "no supplier for this account")
		return
	}
	if err != nil {
		writeSupplierStoreError(w, "supplier status", err)
		return
	}

	if stripeKey() != "" && acct != "" {
		out, gerr := stripeGet(r.Context(), "accounts/"+url.PathEscape(acct))
		if gerr != nil {
			writeErr(w, http.StatusServiceUnavailable, "reading Stripe connected account status")
			return
		}
		pe, gerr := parseStripeConnectAccountStatus(out, acct)
		if gerr != nil {
			writeErr(w, http.StatusServiceUnavailable, "Stripe connected account status is unavailable")
			return
		}
		payoutsEnabled = pe
	}

	status := "none"
	if acct != "" {
		if payoutsEnabled {
			status = "enabled"
		} else {
			status = "pending"
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"supplier_id":     supplierID,
		"connect_status":  status,
		"payouts_enabled": payoutsEnabled,
		"kyc_provider":    "stripe_connect",
	})
}

// stripeConnectedAccountMismatch is the Connect envelope check: the event-level
// account and the object id must name the same connected account when both are
// present. Extracted so the non-Connect refusal matrix can fire this path
// without standing up a Store.
func stripeConnectedAccountMismatch(envelopeAccount, objectID string) bool {
	return envelopeAccount != "" && objectID != "" && envelopeAccount != objectID
}

func (s *Server) handleConnectWebhook(w http.ResponseWriter, r *http.Request) {
	if !s.requireOperationalControlActive(w, r, controlWebhooks) ||
		!s.requireOperationalControlActive(w, r, controlPayments) {
		return
	}
	authority, err := authorizePaymentOperation(
		paymentOperationWebhook, 0, "", stripeKey(),
	)
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, "connect webhook authority is unavailable")
		return
	}
	secret := os.Getenv("MERC_CONNECT_WEBHOOK_SECRET")
	if secret == "" {
		writeErr(w, http.StatusServiceUnavailable, "connect webhooks not configured (set MERC_CONNECT_WEBHOOK_SECRET)")
		return
	}
	payload, err := readStripeWebhookPayload(r.Body)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "unreadable Stripe webhook body")
		return
	}
	if !verifyStripeSig(payload, r.Header.Get("Stripe-Signature"), secret) {
		writeErr(w, http.StatusBadRequest, "invalid stripe signature")
		return
	}
	var ev struct {
		ID         string `json:"id"`
		Type       string `json:"type"`
		Account    string `json:"account"`
		Created    int64  `json:"created"`
		APIVersion string `json:"api_version"`
		Livemode   *bool  `json:"livemode"`
		Data       struct {
			Object map[string]any `json:"object"`
		} `json:"data"`
	}
	if err := json.Unmarshal(payload, &ev); err != nil {
		writeErr(w, http.StatusBadRequest, "unparseable webhook body")
		return
	}
	if err := validateStripeEventContract(
		ev.APIVersion, ev.Livemode, authority.Mode == PaymentModeLive,
	); err != nil {
		writeErr(w, http.StatusBadRequest, "connect webhook contract mismatch")
		return
	}
	if !isStripeConnectEventType(ev.Type) {
		w.Header().Set("X-Merc-Stripe-Event-Outcome", "accepted")
		w.WriteHeader(http.StatusOK)
		return
	}
	object := ev.Data.Object
	if object == nil {
		writeErr(w, http.StatusBadRequest, "unparseable Connect webhook object")
		return
	}
	event, err := parseStripeConnectEvent(ev.ID, ev.Type, ev.Account, ev.Created, object, payload)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	result, err := s.store.ApplyConnectWebhookEvent(r.Context(), event)
	if errors.Is(err, errInvalidStripeConnectEvent) || errors.Is(err, errUnknownConnectAccount) {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err != nil {
		log.Printf("connect webhook: event apply failed type=%s event=%s: %v", ev.Type, ev.ID, err)
		writeErr(w, http.StatusInternalServerError, "recording Connect webhook event")
		return
	}
	outcome := "recorded"
	if result.Duplicate {
		outcome = "duplicate"
	} else if result.Stale {
		outcome = "stale_ignored"
	}
	w.Header().Set("X-Merc-Stripe-Event-Outcome", outcome)
	w.WriteHeader(http.StatusOK)
}
