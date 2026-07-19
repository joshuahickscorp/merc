package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"

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
		acct = *acctP
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

func (s *Store) SetSupplierPayoutsEnabledByAcct(ctx context.Context, acct string, enabled bool) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE suppliers SET payouts_enabled = $2 WHERE stripe_acct = $1`, acct, enabled)
	return err
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
	default:
		writeErr(w, http.StatusInternalServerError, action+": "+err.Error())
	}
}

func (s *Server) handleSupplierOnboard(w http.ResponseWriter, r *http.Request) {
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
	if err := decodeEmptySupplierBody(r); err != nil {
		writeErr(w, http.StatusBadRequest, "supplier identity comes from the authenticated account; send an empty JSON object")
		return
	}
	auth := r.Context().Value(ctxBuyer).(*AuthResult)
	supplierID, err := s.store.EnsureSupplierForBuyer(r.Context(), auth.BuyerID)
	if err != nil {
		writeSupplierStoreError(w, "recording supplier", err)
		return
	}
	workerID := uuid.New()
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
		if out, gerr := stripeGet(r.Context(), "accounts/"+acct); gerr == nil {
			if pe, ok := out["payouts_enabled"].(bool); ok {
				payoutsEnabled = pe
				_ = s.store.SetSupplierPayoutsEnabledByAcct(r.Context(), acct, pe) // keep cache fresh
			}
		}
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

func (s *Server) handleConnectWebhook(w http.ResponseWriter, r *http.Request) {
	secret := os.Getenv("CX_CONNECT_WEBHOOK_SECRET")
	if secret == "" {
		secret = os.Getenv("STRIPE_WEBHOOK_SECRET")
	}
	if secret == "" {
		writeErr(w, http.StatusServiceUnavailable, "connect webhooks not configured (set CX_CONNECT_WEBHOOK_SECRET or STRIPE_WEBHOOK_SECRET)")
		return
	}
	payload, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if !verifyStripeSig(payload, r.Header.Get("Stripe-Signature"), secret) {
		writeErr(w, http.StatusBadRequest, "invalid stripe signature")
		return
	}
	var ev struct {
		Type string `json:"type"`
		Data struct {
			Object map[string]any `json:"object"`
		} `json:"data"`
	}
	if err := json.Unmarshal(payload, &ev); err != nil {
		writeErr(w, http.StatusBadRequest, "unparseable webhook body")
		return
	}
	if ev.Type == "account.updated" {
		obj := ev.Data.Object
		acct, _ := obj["id"].(string)
		pe, _ := obj["payouts_enabled"].(bool)
		if acct != "" {
			if err := s.store.SetSupplierPayoutsEnabledByAcct(r.Context(), acct, pe); err != nil {
				writeErr(w, http.StatusInternalServerError, "updating payout readiness: "+err.Error())
				return
			}
		}
	}
	w.WriteHeader(http.StatusOK)
}
